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
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	var err error

	// Set up logging
	logrus.SetOutput(&logWriter{})
	internal.SetupStdLogging()

	// Strip file:// URI scheme if present (React Native passes URIs, not paths)
	m.StorageDirectory = stripFileURIScheme(m.StorageDirectory)
	m.CacheDirectory = stripFileURIScheme(m.CacheDirectory)

	// Ensure storage directory exists
	if err := os.MkdirAll(m.StorageDirectory, 0700); err != nil {
		logrus.WithError(err).Fatal("Failed to create storage directory")
		return 0
	}

	// Ensure cache directory exists
	if m.CacheDirectory != "" {
		if err := os.MkdirAll(m.CacheDirectory, 0700); err != nil {
			logrus.WithError(err).Warn("Failed to create cache directory")
		}
	}

	// Generate or load server key
	keyPath := filepath.Join(m.StorageDirectory, "matrix_key.pem")
	var privateKey ed25519.PrivateKey
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		_, privateKey, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			logrus.WithError(err).Fatal("Failed to generate key")
			return 0
		}
		// Save the key
		if err := savePrivateKey(keyPath, privateKey); err != nil {
			logrus.WithError(err).Warn("Failed to save private key")
		}
	} else {
		privateKey, err = loadPrivateKey(keyPath)
		if err != nil {
			logrus.WithError(err).Fatal("Failed to load private key")
			return 0
		}
	}

	// Generate tokens for appservices
	m.tokens = GenerateAppserviceTokens()
	logrus.Info("Generated appservice tokens")

	// Create configuration
	m.cfg = generateConfig(m.StorageDirectory, m.CacheDirectory, privateKey)

	// Register appservices BEFORE deriving config
	// This adds WhatsApp bridge and double puppet appservices
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
		false, // disable metrics
	)

	// Create NATS instance
	natsInstance := jetstream.NATSInstance{}

	// Create federation client (even though we won't federate)
	federationClient := fclient.NewFederationClient(
		m.cfg.Global.SigningIdentities(),
		fclient.WithSkipVerify(true),
	)

	// Initialize room server
	rsAPI := roomserver.NewInternalAPI(m.processCtx, m.cfg, cm, &natsInstance, caches, false)

	// Initialize federation API (required even for local-only)
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

	// Create listener on random port
	m.listener, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		logrus.WithError(err).Fatal("Failed to create listener")
		return 0
	}

	// Create HTTP server
	m.httpServer = &http.Server{
		Handler:      routers.Client,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	// Start serving
	go func() {
		logrus.Infof("Dendrite listening on %s", m.listener.Addr().String())
		if err := m.httpServer.Serve(m.listener); err != nil && err != http.ErrServerClosed {
			logrus.WithError(err).Error("HTTP server error")
		}
	}()

	port := m.listener.Addr().(*net.TCPAddr).Port

	// Start WhatsApp bridge after Dendrite is fully running
	go m.startWhatsAppBridge(port)

	return port
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

	// Lower bcrypt cost for mobile
	cfg.UserAPI.BCryptCost = 4

	// Media storage
	mediaPath := filepath.Join(storageDir, "media")
	os.MkdirAll(mediaPath, 0700)
	cfg.MediaAPI.BasePath = config.Path(mediaPath)
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
