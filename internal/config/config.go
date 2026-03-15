package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	MySQLHost    string
	MySQLPort    int
	MySQLUser    string
	MySQLPass    string
	Namespace    string
	ListenAddr   string
	ServiceToken string

	// Delivery behaviour
	PollIntervalMs  int // how often to poll for queued requests
	DeliveryTimeout int // HTTP timeout per webhook call (seconds)
	MaxConcurrent   int // max concurrent deliveries
	ErrorBackoffSec int // backoff window when a webhook has too many errors
	ErrorThreshold  int // number of errors within backoff window to trigger backoff
}

func LoadFromEnv() *Config {
	return &Config{
		MySQLHost:       envStr("MYSQL_HOST", "127.0.0.1"),
		MySQLPort:       envInt("MYSQL_PORT", 3306),
		MySQLUser:       envStr("MYSQL_USER", "phorge"),
		MySQLPass:       envStr("MYSQL_PASS", ""),
		Namespace:       envStr("STORAGE_NAMESPACE", "phorge"),
		ListenAddr:      envStr("LISTEN_ADDR", ":8160"),
		ServiceToken:    envStr("SERVICE_TOKEN", ""),
		PollIntervalMs:  envInt("POLL_INTERVAL_MS", 1000),
		DeliveryTimeout: envInt("DELIVERY_TIMEOUT", 15),
		MaxConcurrent:   envInt("MAX_CONCURRENT", 8),
		ErrorBackoffSec: envInt("ERROR_BACKOFF_SEC", 300),
		ErrorThreshold:  envInt("ERROR_THRESHOLD", 10),
	}
}

func (c *Config) HeraldDSN() string {
	return fmt.Sprintf(
		"%s:%s@tcp(%s:%d)/%s_herald?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s",
		c.MySQLUser, c.MySQLPass, c.MySQLHost, c.MySQLPort, c.Namespace,
	)
}

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	}
	return fallback
}
