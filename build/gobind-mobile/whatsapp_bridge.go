// Copyright 2025 Muhammad Ali
// SPDX-License-Identifier: MIT

package gobind

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"

	"go.mau.fi/mautrix-whatsapp/pkg/connector"
)

// WhatsAppBridge wraps the mautrix-whatsapp bridge for embedded use
type WhatsAppBridge struct {
	bridgeMain *mxmain.BridgeMain
	ctx        context.Context
	cancel     context.CancelFunc
	running    bool
	mu         sync.Mutex
}

// NewWhatsAppBridge creates a new WhatsApp bridge instance
func NewWhatsAppBridge() *WhatsAppBridge {
	return &WhatsAppBridge{
		bridgeMain: &mxmain.BridgeMain{
			Name:        "mautrix-whatsapp",
			URL:         "https://github.com/mautrix/whatsapp",
			Description: "A Matrix-WhatsApp puppeting bridge (embedded)",
			Version:     "0.1.0-embedded",
			Connector:   &connector.WhatsAppConnector{},
		},
	}
}

// Start initializes and starts the WhatsApp bridge
func (wb *WhatsAppBridge) Start(configPath string) error {
	wb.mu.Lock()
	defer wb.mu.Unlock()

	if wb.running {
		return fmt.Errorf("bridge already running")
	}

	wb.ctx, wb.cancel = context.WithCancel(context.Background())

	// Set config path directly on BridgeMain instead of using CLI flag parsing
	// This is more reliable for embedded mobile use
	wb.bridgeMain.ConfigPath = configPath
	wb.bridgeMain.SaveConfig = false // Don't try to update/save config file

	// Initialize the bridge
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logrus.Errorf("WhatsApp bridge panicked: %v", r)
			}
		}()

		// Load config directly (bypasses PreInit's flag parsing)
		wb.bridgeMain.LoadConfig()

		// Init sets up database, logging, etc.
		wb.bridgeMain.Init()

		// Dendrite reports spec v1.2 but bridge wants v1.4
		// Dendrite supports most v1.4 features, just doesn't advertise it
		if wb.bridgeMain.Matrix != nil {
			wb.bridgeMain.Matrix.IgnoreUnsupportedServer = true
		}

		wb.mu.Lock()
		wb.running = true
		wb.mu.Unlock()

		logrus.Info("WhatsApp bridge initialized, starting...")

		// Start runs the bridge and blocks
		wb.bridgeMain.Start()

		// Wait for context cancellation
		<-wb.ctx.Done()
	}()

	return nil
}

// Stop gracefully shuts down the WhatsApp bridge
func (wb *WhatsAppBridge) Stop() {
	wb.mu.Lock()
	defer wb.mu.Unlock()

	if !wb.running {
		return
	}

	logrus.Info("Stopping WhatsApp bridge...")

	if wb.cancel != nil {
		wb.cancel()
	}

	if wb.bridgeMain != nil {
		wb.bridgeMain.Stop()
	}

	wb.running = false
	logrus.Info("WhatsApp bridge stopped")
}

// IsRunning returns whether the bridge is currently running
func (wb *WhatsAppBridge) IsRunning() bool {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	return wb.running
}

// WaitForReady waits until the bridge HTTP server is responding
func (wb *WhatsAppBridge) WaitForReady(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// TODO: Check if bridge HTTP server is responding
		// For now, just wait a bit for the bridge to start
		time.Sleep(500 * time.Millisecond)
		if wb.IsRunning() {
			return true
		}
	}
	return false
}

// generateBridgeConfig creates the mautrix-whatsapp configuration
func generateBridgeConfig(dataPath string, dendritePort int, tokens *AppserviceTokens) string {
	return fmt.Sprintf(`# Auto-generated mautrix-whatsapp config for embedded use
network:
    os_name: Cirano Mobile
    browser_name: unknown
    displayname_template: "{{.Phone}} [{{or .FullName .PushName .BusinessName}}] (WhatsApp)"
    history_sync:
        max_initial_conversations: 20
        request_full_sync: true
        dispatch_wait: 5s
        full_sync_config:
            days_limit: 365
            size_mb_limit: null
            storage_quota_mb: null
        media_requests:
            auto_request_media: false
            request_method: immediate
            request_local_time: 120
            max_async_handle: 1
    call_start_notices: false
    identity_change_notices: false
    send_presence_on_typing: false

bridge:
    command_prefix: '!wa'
    personal_filtering_spaces: true
    private_chat_portal_meta: true
    permissions:
        '*': user
        '@whatsappbot:localhost': admin

database:
    type: sqlite3-fk-wal
    uri: file:%s/whatsapp/bridge.db?_txlock=immediate

homeserver:
    address: http://127.0.0.1:%d
    domain: localhost
    software: standard

appservice:
    address: http://127.0.0.1:29318
    hostname: 127.0.0.1
    port: 29318
    id: whatsapp
    bot:
        username: whatsappbot
        displayname: WhatsApp Bridge
    ephemeral_events: true
    as_token: %s
    hs_token: %s
    username_template: whatsapp_{{.}}

matrix:
    federate_rooms: false
    sync_direct_chat_list: true
    message_status_events: false
    delivery_receipts: false
    message_error_notices: true

double_puppet:
    servers: {}
    allow_discovery: false
    secrets:
        localhost: "as_token:%s"

encryption:
    allow: false
    default: false

backfill:
    enabled: true
    max_initial_messages: 100
    max_catchup_messages: 100
    unread_hours_threshold: -1
    threads:
        max_initial_messages: 100
    queue:
        enabled: true
        batch_size: 100
        batch_delay: 20
        max_batches: -1

logging:
    min_level: info
    writers:
        - type: stdout
          format: pretty-colored
`, dataPath, dendritePort, tokens.WAASToken, tokens.WAHSToken, tokens.DPASToken)
}

// writeBridgeConfig writes the bridge configuration to disk
func writeBridgeConfig(storageDir string, dendritePort int, tokens *AppserviceTokens) (string, error) {
	// Create whatsapp data directory
	waDataPath := filepath.Join(storageDir, "whatsapp")
	if err := os.MkdirAll(waDataPath, 0700); err != nil {
		return "", fmt.Errorf("failed to create whatsapp directory: %w", err)
	}

	// Write bridge config
	configPath := filepath.Join(waDataPath, "config.yaml")
	configContent := generateBridgeConfig(storageDir, dendritePort, tokens)

	if err := os.WriteFile(configPath, []byte(configContent), 0600); err != nil {
		return "", fmt.Errorf("failed to write bridge config: %w", err)
	}

	logrus.Infof("Wrote WhatsApp bridge config to %s", configPath)
	return configPath, nil
}
