package config

import (
	"fmt"
	"os"
)

type Config struct {
	Port           string
	BaseURL        string
	OIDCIssuerURL  string
	OIDCClientID   string
	OIDCClientSecret string
	SessionSecret  string
	PodNamespace   string
	PodImage       string
	PodShell       string
	PodCPULimit    string
	PodMemoryLimit string
	Kubeconfig     string
}

func Load() (*Config, error) {
	c := &Config{
		Port:           getEnv("PORT", "8080"),
		BaseURL:        getEnv("BASE_URL", "https://run.pmh.codes"),
		OIDCIssuerURL:  os.Getenv("OIDC_ISSUER_URL"),
		OIDCClientID:   os.Getenv("OIDC_CLIENT_ID"),
		OIDCClientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
		SessionSecret:  os.Getenv("SESSION_SECRET"),
		PodNamespace:   getEnv("POD_NAMESPACE", "run"),
		PodImage:       os.Getenv("POD_IMAGE"),
		PodShell:       getEnv("POD_SHELL", "/bin/bash"),
		PodCPULimit:    getEnv("POD_CPU_LIMIT", "500m"),
		PodMemoryLimit: getEnv("POD_MEMORY_LIMIT", "256Mi"),
		Kubeconfig:     os.Getenv("KUBECONFIG"),
	}

	required := map[string]string{
		"OIDC_ISSUER_URL":    c.OIDCIssuerURL,
		"OIDC_CLIENT_ID":     c.OIDCClientID,
		"OIDC_CLIENT_SECRET": c.OIDCClientSecret,
		"SESSION_SECRET":     c.SessionSecret,
		"POD_IMAGE":          c.PodImage,
	}
	for k, v := range required {
		if v == "" {
			return nil, fmt.Errorf("required env var %s is not set", k)
		}
	}

	if len(c.SessionSecret) < 32 {
		return nil, fmt.Errorf("SESSION_SECRET must be at least 32 characters")
	}

	return c, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
