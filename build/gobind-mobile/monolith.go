// Copyright 2024 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// Package gobind provides gomobile bindings for embedding Dendrite on mobile devices.
// This is a simplified version without P2P/Pinecone - just a local HTTP server.
package gobind

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/element-hq/dendrite/appservice"
	"github.com/element-hq/dendrite/clientapi/userutil"
	"github.com/element-hq/dendrite/federationapi"
	"github.com/element-hq/dendrite/internal"
	"github.com/element-hq/dendrite/internal/caching"
	"github.com/element-hq/dendrite/internal/httputil"
	"github.com/element-hq/dendrite/internal/sqlutil"
	"github.com/element-hq/dendrite/roomserver"
	"github.com/element-hq/dendrite/setup"
	"github.com/element-hq/dendrite/setup/config"
	"github.com/element-hq/dendrite/setup/jetstream"
	"github.com/element-hq/dendrite/setup/process"
	"github.com/element-hq/dendrite/userapi"
	userapiAPI "github.com/element-hq/dendrite/userapi/api"
	"github.com/gorilla/mux"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"

	_ "golang.org/x/mobile/bind"
)

// DendriteMonolith represents an embedded Dendrite Matrix server.
type DendriteMonolith struct {
	StorageDirectory string
	CacheDirectory   string

	listener       net.Listener
	httpServer     *http.Server
	processCtx     *process.ProcessContext
	userAPI        userapiAPI.UserInternalAPI
	cfg            *config.Dendrite
	whatsappBridge *WhatsAppBridge
	tokens         *AppserviceTokens
}

// Start initializes and starts the Dendrite server.
// Returns the port number the server is listening on.
func (m *DendriteMonolith) Start() int {
	// Set up logging (cheap)
	logrus.SetOutput(&logWriter{})
	internal.SetupStdLogging()

	// Strip file:// URI scheme if present (React Native passes URIs, not paths)
	m.StorageDirectory = stripFileURIScheme(m.StorageDirectory)
	m.CacheDirectory = stripFileURIScheme(m.CacheDirectory)

	// Ensure storage directory exists (cheap)
	if err := os.MkdirAll(m.StorageDirectory, 0700); err != nil {
		logrus.WithError(err).Fatal("Failed to create storage directory")
		return 0
	}
	if m.CacheDirectory != "" {
		if err := os.MkdirAll(m.CacheDirectory, 0700); err != nil {
			logrus.WithError(err).Warn("Failed to create cache directory")
		}
	}

	// Open the TCP listener BEFORE the heavy work so we have a port to return.
	// The kernel will queue any incoming connections in the backlog until the HTTP
	// server starts accepting them inside the async-init goroutine below. JS health
	// polling (waitForServerReady) handles the brief gap.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		logrus.WithError(err).Fatal("Failed to create listener")
		return 0
	}
	m.listener = listener
	port := listener.Addr().(*net.TCPAddr).Port

	// Defer the heavy initialisation (key gen, SQLite migrations, route setup, bridge
	// start) to a goroutine so we can return the port immediately and unblock the
	// JS thread on the iOS side. On a fresh install this work takes 5–8 seconds.
	go m.asyncStart(port)

	return port
}

// asyncStart performs the slow part of bootstrap (key/config/SQLite migrations,
// HTTP route registration, bridge launch) on a background goroutine.
func (m *DendriteMonolith) asyncStart(port int) {
	var err error

	// Generate or load server key
	keyPath := filepath.Join(m.StorageDirectory, "matrix_key.pem")
	var privateKey ed25519.PrivateKey
	if _, err = os.Stat(keyPath); os.IsNotExist(err) {
		_, privateKey, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			logrus.WithError(err).Error("Failed to generate key")
			return
		}
		if err := savePrivateKey(keyPath, privateKey); err != nil {
			logrus.WithError(err).Warn("Failed to save private key")
		}
	} else {
		privateKey, err = loadPrivateKey(keyPath)
		if err != nil {
			logrus.WithError(err).Error("Failed to load private key")
			return
		}
	}

	// Generate tokens for appservices
	m.tokens = GenerateAppserviceTokens()
	logrus.Info("Generated appservice tokens")

	// Create configuration
	m.cfg = generateConfig(m.StorageDirectory, m.CacheDirectory, privateKey)

	// Register appservices BEFORE deriving config (writes registration files).
	if err := registerAppservices(m.cfg, m.tokens); err != nil {
		logrus.WithError(err).Error("Failed to register appservices")
		// Continue anyway - Dendrite will work, just without bridge
	}

	// Initialize process context
	m.processCtx = process.NewProcessContext()

	// Create connection manager
	cm := sqlutil.NewConnectionManager(m.processCtx, m.cfg.Global.DatabaseOptions)

	// Create routers
	routers := httputil.NewRouters()

	// Create caches
	caches := caching.NewRistrettoCache(
		m.cfg.Global.Cache.EstimatedMaxSize,
		m.cfg.Global.Cache.MaxAge,
		false,
	)

	// Create NATS instance
	natsInstance := jetstream.NATSInstance{}

	// Create federation client (even though we won't federate)
	federationClient := fclient.NewFederationClient(
		m.cfg.Global.SigningIdentities(),
		fclient.WithSkipVerify(true),
	)

	// Initialize room server (opens SQLite, runs migrations — slow on cold start)
	rsAPI := roomserver.NewInternalAPI(m.processCtx, m.cfg, cm, &natsInstance, caches, false)

	// Initialize federation API
	fsAPI := federationapi.NewInternalAPI(
		m.processCtx, m.cfg, cm, &natsInstance, federationClient, rsAPI, caches, nil, false,
	)

	keyRing := fsAPI.KeyRing()
	rsAPI.SetFederationAPI(fsAPI, keyRing)

	// Initialize user API
	m.userAPI = userapi.NewInternalAPI(
		m.processCtx, m.cfg, cm, &natsInstance, rsAPI, federationClient, false, fsAPI.IsBlacklistedOrBackingOff,
	)

	// Initialize appservice API
	asAPI := appservice.NewInternalAPI(m.processCtx, m.cfg, &natsInstance, m.userAPI, rsAPI)

	rsAPI.SetAppserviceAPI(asAPI)
	rsAPI.SetUserAPI(m.userAPI)

	// Create monolith
	monolith := setup.Monolith{
		Config:        m.cfg,
		Client:        fclient.NewClient(fclient.WithSkipVerify(true)),
		FedClient:     federationClient,
		KeyRing:       keyRing,
		AppserviceAPI: asAPI,
		FederationAPI: fsAPI,
		RoomserverAPI: rsAPI,
		UserAPI:       m.userAPI,
	}

	// Add all public routes
	monolith.AddAllPublicRoutes(m.processCtx, m.cfg, routers, cm, &natsInstance, caches, false)

	// Create combined router for Client + Media APIs
	httpRouter := mux.NewRouter().SkipClean(true).UseEncodedPath()
	httpRouter.PathPrefix(httputil.PublicClientPathPrefix).Handler(routers.Client)
	// httpRouter.PathPrefix(httputil.PublicMediaPathPrefix).Handler(routers.Media)

	// Create HTTP server
	m.httpServer = &http.Server{
		Handler:      httpRouter,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	// Start serving — this drains any TCP connections the kernel queued while we
	// were initialising, and unblocks JS waitForServerReady polling.
	go func() {
		logrus.Infof("Dendrite listening on %s", m.listener.Addr().String())
		if err := m.httpServer.Serve(m.listener); err != nil && err != http.ErrServerClosed {
			logrus.WithError(err).Error("HTTP server error")
		}
	}()

	// Start WhatsApp bridge once Dendrite is actually responsive — not just listening.
	// The bridgev2 framework dispatches all history-sync portals in parallel (53+ at once
	// in our case), and a cold-started Dendrite can't service that burst within the bridge's
	// per-action context timeout (~15s), causing m.room.name events to silently fail.
	// Warming Dendrite before the bridge connects to WhatsApp prevents the cascade.
	go func() {
		m.waitForDendriteWarm(port)
		m.startWhatsAppBridge(port)
	}()
}

// waitForDendriteWarm blocks until Dendrite is genuinely warm enough to handle the
// bridge's parallel portal-creation burst (50+ concurrent /createRoom + state-event
// calls). A "warm" state means:
//
//   1. /_matrix/client/versions responds fast 5 times in a row (the listener and basic
//      router are up and not stalled on SQLite migrations).
//   2. A dwell period has elapsed since first responsiveness so heavier code paths
//      (caching layer, NATS streams, sqlite query plans) get a chance to settle.
//
// Probing /versions alone is not enough: it's served by a static handler that responds
// in microseconds even while the room/user APIs are mid-migration. The dwell period
// accounts for that asymmetry. Capped at 60s so we never block forever.
func (m *DendriteMonolith) waitForDendriteWarm(port int) {
	const (
		fastResponseThreshold = 200 * time.Millisecond
		consecutiveFastNeeded = 5
		minDwellAfterFirstOK  = 8 * time.Second
		probeCadence          = 500 * time.Millisecond
		maxTotalWait          = 60 * time.Second
	)
	url := fmt.Sprintf("http://127.0.0.1:%d/_matrix/client/versions", port)
	client := &http.Client{Timeout: 5 * time.Second}
	consecutiveFast := 0
	deadline := time.Now().Add(maxTotalWait)
	var firstResponsive time.Time

	for time.Now().Before(deadline) {
		start := time.Now()
		resp, err := client.Get(url)
		dur := time.Since(start)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				if firstResponsive.IsZero() {
					firstResponsive = start
				}
				if dur < fastResponseThreshold {
					consecutiveFast++
				} else {
					consecutiveFast = 0
				}
			} else {
				consecutiveFast = 0
			}
		} else {
			consecutiveFast = 0
		}

		dwellOK := !firstResponsive.IsZero() && time.Since(firstResponsive) >= minDwellAfterFirstOK
		if consecutiveFast >= consecutiveFastNeeded && dwellOK {
			logrus.Infof(
				"Dendrite warm: %d consecutive fast probes, dwell %s since first OK — starting bridge",
				consecutiveFast, time.Since(firstResponsive).Round(time.Millisecond),
			)
			return
		}
		time.Sleep(probeCadence)
	}
	logrus.Warn("Dendrite did not reach warm state within timeout; starting bridge anyway")
}

// startWhatsAppBridge initializes and starts the mautrix-whatsapp bridge
func (m *DendriteMonolith) startWhatsAppBridge(dendritePort int) {
	logrus.Info("Starting WhatsApp bridge...")

	// Write bridge config
	configPath, err := writeBridgeConfig(m.StorageDirectory, dendritePort, m.tokens)
	if err != nil {
		logrus.WithError(err).Error("Failed to write bridge config")
		return
	}

	// Create and start bridge
	m.whatsappBridge = NewWhatsAppBridge()
	if err := m.whatsappBridge.Start(configPath); err != nil {
		logrus.WithError(err).Error("Failed to start WhatsApp bridge")
		return
	}

	logrus.Info("WhatsApp bridge started successfully")
}

// Stop gracefully shuts down the Dendrite server and WhatsApp bridge.
func (m *DendriteMonolith) Stop() {
	// Stop WhatsApp bridge first
	if m.whatsappBridge != nil {
		m.whatsappBridge.Stop()
	}

	if m.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m.httpServer.Shutdown(ctx)
	}
	if m.processCtx != nil {
		m.processCtx.ShutdownDendrite()
		m.processCtx.WaitForComponentsToFinish()
	}
}

// Restart stops and restarts the Dendrite server and WhatsApp bridge.
// Returns the new port number the server is listening on.
func (m *DendriteMonolith) Restart() int {
	m.Stop()
	return m.Start()
}

// IsWhatsAppBridgeRunning returns whether the WhatsApp bridge is running
func (m *DendriteMonolith) IsWhatsAppBridgeRunning() bool {
	if m.whatsappBridge == nil {
		return false
	}
	return m.whatsappBridge.IsRunning()
}

// GetWhatsAppBotUserID returns the Matrix user ID of the WhatsApp bridge bot
func (m *DendriteMonolith) GetWhatsAppBotUserID() string {
	return "@whatsappbot:localhost"
}

// VersionInfo contains version information for all components
type VersionInfo struct {
	Dendrite       string `json:"dendrite"`
	WhatsAppBridge string `json:"whatsapp_bridge"`
	Whatsmeow      string `json:"whatsmeow"`
	Mautrix        string `json:"mautrix"`
}

// GetVersionInfo returns version information for Dendrite and bridges as JSON
func (m *DendriteMonolith) GetVersionInfo() string {
	info := VersionInfo{
		Dendrite:       internal.VersionString(),
		WhatsAppBridge: "unknown",
		Whatsmeow:      "unknown",
		Mautrix:        "unknown",
	}

	// Get module versions from build info
	if buildInfo, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range buildInfo.Deps {
			switch dep.Path {
			case "go.mau.fi/mautrix-whatsapp":
				info.WhatsAppBridge = dep.Version
			case "go.mau.fi/whatsmeow":
				info.Whatsmeow = dep.Version
			case "maunium.net/go/mautrix":
				info.Mautrix = dep.Version
			}
		}
	}

	jsonBytes, err := json.Marshal(info)
	if err != nil {
		return `{"error": "failed to marshal version info"}`
	}
	return string(jsonBytes)
}

// GetWhatsAppBridgeConfig returns the current WhatsApp bridge config YAML.
// Returns the config file contents from disk, or an error message if unavailable.
func (m *DendriteMonolith) GetWhatsAppBridgeConfig() string {
	if m.StorageDirectory == "" {
		return "error: storage directory not set"
	}
	configPath := filepath.Join(m.StorageDirectory, "whatsapp", "config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	return string(data)
}

// GetBridgeLogPath returns the absolute path to the WhatsApp bridge log file.
// The log file is created when the bridge starts; this just returns where it lives.
func (m *DendriteMonolith) GetBridgeLogPath() string {
	if m.StorageDirectory == "" {
		return ""
	}
	return filepath.Join(m.StorageDirectory, "whatsapp", "bridge.log")
}

// GetBridgeDBStats opens the bridge SQLite DB read-only and returns counts +
// a list of portals with their Matrix room IDs. Returns JSON.
func (m *DendriteMonolith) GetBridgeDBStats() string {
	if m.StorageDirectory == "" {
		return `{"error":"storage directory not set"}`
	}
	dbPath := filepath.Join(m.StorageDirectory, "whatsapp", "bridge.db")
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Sprintf(`{"error":"bridge.db not found: %v"}`, err)
	}

	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro&immutable=0")
	if err != nil {
		return fmt.Sprintf(`{"error":"open: %v"}`, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	result := map[string]interface{}{}

	// list of tables in DB
	tables := []string{}
	if rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name"); err == nil {
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err == nil {
				tables = append(tables, n)
			}
		}
		rows.Close()
	}
	result["tables"] = tables

	tableSet := map[string]bool{}
	for _, t := range tables {
		tableSet[t] = true
	}

	counts := map[string]int{}
	countQ := func(label, q string) {
		var n int
		if err := db.QueryRow(q).Scan(&n); err == nil {
			counts[label] = n
		} else {
			counts[label] = -1
		}
	}
	if tableSet["portal"] {
		countQ("portal_total", "SELECT COUNT(*) FROM portal")
		countQ("portal_with_mxid", "SELECT COUNT(*) FROM portal WHERE mxid IS NOT NULL AND mxid != ''")
	}
	if tableSet["ghost"] {
		countQ("ghost_total", "SELECT COUNT(*) FROM ghost")
	}
	if tableSet["message"] {
		countQ("message_total", "SELECT COUNT(*) FROM message")
	}
	if tableSet["user_login"] {
		countQ("user_login_total", "SELECT COUNT(*) FROM user_login")
	}
	if tableSet["whatsmeow_contacts"] {
		countQ("whatsmeow_contacts_total", "SELECT COUNT(*) FROM whatsmeow_contacts")
	}
	if tableSet["whatsmeow_chat_settings"] {
		countQ("whatsmeow_chat_settings_total", "SELECT COUNT(*) FROM whatsmeow_chat_settings")
	}
	result["counts"] = counts

	// list portals
	type portalRow struct {
		ID          string `json:"id"`
		Receiver    string `json:"receiver"`
		MXID        string `json:"mxid"`
		Name        string `json:"name"`
		OtherUserID string `json:"other_user_id"`
		RoomType    string `json:"room_type"`
	}
	portals := []portalRow{}
	if tableSet["portal"] {
		// schema may have different columns across versions; use fault-tolerant SELECT
		rows, err := db.Query(`SELECT
			COALESCE(id, ''),
			COALESCE(receiver, ''),
			COALESCE(mxid, ''),
			COALESCE(name, ''),
			COALESCE(other_user_id, ''),
			COALESCE(room_type, '')
			FROM portal ORDER BY id`)
		if err == nil {
			for rows.Next() {
				var p portalRow
				if err := rows.Scan(&p.ID, &p.Receiver, &p.MXID, &p.Name, &p.OtherUserID, &p.RoomType); err == nil {
					portals = append(portals, p)
				}
			}
			rows.Close()
		} else {
			result["portal_query_error"] = err.Error()
		}
	}
	result["portals"] = portals

	out, _ := json.Marshal(result)
	return string(out)
}

// BaseURL returns the base URL of the running server.
func (m *DendriteMonolith) BaseURL() string {
	if m.listener == nil {
		return ""
	}
	return fmt.Sprintf("http://%s", m.listener.Addr().String())
}

// RegisterUser creates a new user account.
func (m *DendriteMonolith) RegisterUser(localpart, password string) (string, error) {
	if m.userAPI == nil {
		return "", fmt.Errorf("server not started")
	}

	userID := userutil.MakeUserID(localpart, m.cfg.Global.ServerName)

	req := &userapiAPI.PerformAccountCreationRequest{
		AccountType: userapiAPI.AccountTypeUser,
		Localpart:   localpart,
		Password:    password,
	}
	res := &userapiAPI.PerformAccountCreationResponse{}

	if err := m.userAPI.PerformAccountCreation(context.Background(), req, res); err != nil {
		return "", fmt.Errorf("failed to create account: %w", err)
	}

	return userID, nil
}

// RegisterDevice creates a new device for a user and returns an access token.
func (m *DendriteMonolith) RegisterDevice(localpart, deviceID string) (string, error) {
	if m.userAPI == nil {
		return "", fmt.Errorf("server not started")
	}

	accessTokenBytes := make([]byte, 16)
	if _, err := rand.Read(accessTokenBytes); err != nil {
		return "", fmt.Errorf("failed to generate token: %w", err)
	}

	req := &userapiAPI.PerformDeviceCreationRequest{
		Localpart:   localpart,
		DeviceID:    &deviceID,
		AccessToken: hex.EncodeToString(accessTokenBytes),
	}
	res := &userapiAPI.PerformDeviceCreationResponse{}

	if err := m.userAPI.PerformDeviceCreation(context.Background(), req, res); err != nil {
		return "", fmt.Errorf("failed to create device: %w", err)
	}

	if !res.DeviceCreated {
		return "", fmt.Errorf("device was not created")
	}

	return res.Device.AccessToken, nil
}

// generateConfig creates a Dendrite configuration for mobile use.
// Uses separate SQLite databases like the pinecone demo.
func generateConfig(storageDir, cacheDir string, privateKey ed25519.PrivateKey) *config.Dendrite {
	cfg := &config.Dendrite{}
	cfg.Defaults(config.DefaultOpts{
		Generate:       true,
		SingleDatabase: false, // Use separate SQLite databases
	})

	cfg.Global.ServerName = spec.ServerName("localhost")
	cfg.Global.PrivateKey = privateKey
	cfg.Global.KeyID = gomatrixserverlib.KeyID("ed25519:dendrite")

	// JetStream storage
	jetstreamPath := filepath.Join(storageDir, "jetstream")
	os.MkdirAll(jetstreamPath, 0700)
	cfg.Global.JetStream.StoragePath = config.Path(jetstreamPath)
	cfg.Global.JetStream.InMemory = true

	// Separate SQLite databases (like pinecone demo)
	dbPrefix := filepath.Join(storageDir, "dendrite")
	cfg.UserAPI.AccountDatabase.ConnectionString = config.DataSource(fmt.Sprintf("file:%s-account.db", dbPrefix))
	cfg.MediaAPI.Database.ConnectionString = config.DataSource(fmt.Sprintf("file:%s-mediaapi.db", dbPrefix))
	cfg.SyncAPI.Database.ConnectionString = config.DataSource(fmt.Sprintf("file:%s-syncapi.db", dbPrefix))
	cfg.RoomServer.Database.ConnectionString = config.DataSource(fmt.Sprintf("file:%s-roomserver.db", dbPrefix))
	cfg.KeyServer.Database.ConnectionString = config.DataSource(fmt.Sprintf("file:%s-keyserver.db", dbPrefix))
	cfg.FederationAPI.Database.ConnectionString = config.DataSource(fmt.Sprintf("file:%s-federationapi.db", dbPrefix))
	cfg.RelayAPI.Database.ConnectionString = config.DataSource(fmt.Sprintf("file:%s-relayapi.db", dbPrefix))

	// Enable open registration
	cfg.ClientAPI.RegistrationDisabled = false
	cfg.ClientAPI.OpenRegistrationWithoutVerificationEnabled = true

	// Disable rate limiting for local mobile use
	cfg.ClientAPI.RateLimiting.Enabled = false

	// Lower bcrypt cost for mobile
	cfg.UserAPI.BCryptCost = 4

	// Media storage
	mediaPath := filepath.Join(storageDir, "media")
	os.MkdirAll(mediaPath, 0700)
	cfg.MediaAPI.BasePath = config.Path(mediaPath)
	cfg.MediaAPI.AbsBasePath = config.Path(mediaPath) // Must set AbsBasePath for temp dir creation
	cfg.MediaAPI.MaxFileSizeBytes = config.FileSizeBytes(10 * 1024 * 1024) // 10MB

	// Disable federation
	cfg.FederationAPI.DisableTLSValidation = true
	cfg.FederationAPI.DisableHTTPKeepalives = true

	// Disable full-text search (reduces binary size and complexity)
	cfg.SyncAPI.Fulltext.Enabled = false

	// Cache settings
	cfg.Global.Cache.EstimatedMaxSize = 1024 * 1024 * 16 // 16MB
	cfg.Global.Cache.MaxAge = time.Hour

	// Derive any dependent config
	if err := cfg.Derive(); err != nil {
		logrus.WithError(err).Warn("Failed to derive config")
	}

	return cfg
}

// savePrivateKey saves an Ed25519 private key to a PEM file.
func savePrivateKey(path string, key ed25519.PrivateKey) error {
	// Simple hex encoding for now
	data := hex.EncodeToString(key)
	return os.WriteFile(path, []byte(data), 0600)
}

// loadPrivateKey loads an Ed25519 private key from a PEM file.
func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(string(data))
}

// stripFileURIScheme removes file:// or file: prefix from a path
// React Native on iOS passes URIs like "file:///var/mobile/..." but Go needs plain paths
func stripFileURIScheme(path string) string {
	if strings.HasPrefix(path, "file://") {
		return strings.TrimPrefix(path, "file://")
	}
	if strings.HasPrefix(path, "file:") {
		return strings.TrimPrefix(path, "file:")
	}
	return path
}
