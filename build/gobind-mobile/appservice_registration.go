// Copyright 2025 Muhammad Ali
// SPDX-License-Identifier: MIT

package gobind

import (
	"crypto/rand"
	"encoding/base64"
	"regexp"

	"github.com/element-hq/dendrite/setup/config"
	"github.com/sirupsen/logrus"
)

// AppserviceTokens holds tokens for bridge and double puppet appservices
type AppserviceTokens struct {
	// WhatsApp bridge tokens
	WAASToken string // Application Service token (bridge -> homeserver)
	WAHSToken string // Homeserver token (homeserver -> bridge)
	// Double puppet token (only as_token matters)
	DPASToken string
}

// generateSecureToken creates a cryptographically secure random token
func generateSecureToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		logrus.WithError(err).Warn("Failed to generate secure token, using fallback")
		// Fallback is unlikely but ensures we don't panic
		return "fallback_token_insecure"
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// GenerateAppserviceTokens creates new tokens for all appservices
func GenerateAppserviceTokens() *AppserviceTokens {
	return &AppserviceTokens{
		WAASToken: generateSecureToken(),
		WAHSToken: generateSecureToken(),
		DPASToken: generateSecureToken(),
	}
}

// registerAppservices registers the WhatsApp bridge and double puppet appservices
// This must be called BEFORE cfg.Derive() to ensure regexes are compiled
func registerAppservices(cfg *config.Dendrite, tokens *AppserviceTokens) error {
	logrus.Info("Registering WhatsApp bridge appservice...")

	// 1. WhatsApp Bridge Appservice - handles bridge communication
	waAS := config.ApplicationService{
		ID:              "whatsapp",
		URL:             "http://127.0.0.1:29318",
		ASToken:         tokens.WAASToken,
		HSToken:         tokens.WAHSToken,
		SenderLocalpart: "whatsappbot",
		RateLimited:     false,
		NamespaceMap: map[string][]config.ApplicationServiceNamespace{
			"users": {
				{
					Regex:     "^@whatsappbot:localhost$",
					Exclusive: true,
				},
				{
					Regex:     "^@whatsapp_.*:localhost$",
					Exclusive: true,
				},
			},
		},
	}

	// Compile regexes for the WhatsApp appservice
	for key, namespaces := range waAS.NamespaceMap {
		for i := range namespaces {
			r, err := regexp.Compile(namespaces[i].Regex)
			if err != nil {
				logrus.WithError(err).Errorf("Failed to compile regex for namespace %s", key)
				return err
			}
			namespaces[i].RegexpObject = r
		}
	}

	waAS.CreateHTTPClient(true) // Allow insecure for localhost
	cfg.Derived.ApplicationServices = append(cfg.Derived.ApplicationServices, waAS)
	logrus.Info("Registered WhatsApp bridge appservice")

	// 2. Double Puppet Appservice - allows bridge to act as users
	// URL is empty because homeserver doesn't need to push events to it
	dpAS := config.ApplicationService{
		ID:              "doublepuppet",
		URL:             "",            // Intentionally empty - no callbacks needed
		ASToken:         tokens.DPASToken,
		HSToken:         generateSecureToken(), // Not used but required by spec
		SenderLocalpart: "doublepuppet_placeholder",
		RateLimited:     false,
		NamespaceMap: map[string][]config.ApplicationServiceNamespace{
			"users": {
				{
					Regex:     "@.*:localhost", // Match all users on this server
					Exclusive: false,           // Non-exclusive - doesn't take over users
				},
			},
		},
	}

	// Compile regexes for the double puppet appservice
	for key, namespaces := range dpAS.NamespaceMap {
		for i := range namespaces {
			r, err := regexp.Compile(namespaces[i].Regex)
			if err != nil {
				logrus.WithError(err).Errorf("Failed to compile regex for namespace %s", key)
				return err
			}
			namespaces[i].RegexpObject = r
		}
	}

	dpAS.CreateHTTPClient(true)
	cfg.Derived.ApplicationServices = append(cfg.Derived.ApplicationServices, dpAS)
	logrus.Info("Registered double puppet appservice")

	// Build combined exclusive regex patterns for the homeserver
	// These are used to check if a user/alias is exclusively owned by an appservice
	updateExclusiveRegexps(cfg)

	return nil
}

// updateExclusiveRegexps updates the exclusive regex patterns used by the homeserver
func updateExclusiveRegexps(cfg *config.Dendrite) {
	var exclusiveUsernames []string
	var exclusiveAliases []string

	for _, as := range cfg.Derived.ApplicationServices {
		// Check users namespace
		if users, ok := as.NamespaceMap["users"]; ok {
			for _, ns := range users {
				if ns.Exclusive {
					exclusiveUsernames = append(exclusiveUsernames, "("+ns.Regex+")")
				}
			}
		}
		// Check aliases namespace
		if aliases, ok := as.NamespaceMap["aliases"]; ok {
			for _, ns := range aliases {
				if ns.Exclusive {
					exclusiveAliases = append(exclusiveAliases, "("+ns.Regex+")")
				}
			}
		}
	}

	// Join patterns or use empty pattern
	usernamesPattern := "^$" // Match nothing by default
	aliasesPattern := "^$"

	if len(exclusiveUsernames) > 0 {
		joined := ""
		for i, p := range exclusiveUsernames {
			if i > 0 {
				joined += "|"
			}
			joined += p
		}
		usernamesPattern = joined
	}

	if len(exclusiveAliases) > 0 {
		joined := ""
		for i, p := range exclusiveAliases {
			if i > 0 {
				joined += "|"
			}
			joined += p
		}
		aliasesPattern = joined
	}

	// Compile and store
	var err error
	cfg.Derived.ExclusiveApplicationServicesUsernameRegexp, err = regexp.Compile(usernamesPattern)
	if err != nil {
		logrus.WithError(err).Warn("Failed to compile exclusive usernames regex")
	}

	cfg.Derived.ExclusiveApplicationServicesAliasRegexp, err = regexp.Compile(aliasesPattern)
	if err != nil {
		logrus.WithError(err).Warn("Failed to compile exclusive aliases regex")
	}

	logrus.Debugf("Exclusive usernames pattern: %s", usernamesPattern)
	logrus.Debugf("Exclusive aliases pattern: %s", aliasesPattern)
}
