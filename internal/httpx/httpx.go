// Package httpx provides HTTP transports for long-running processes.
package httpx

import (
	"net/http"
	"time"
)

// Transport returns a clone of http.DefaultTransport that health-checks HTTP/2 connections with
// pings. Without them a connection that died while the machine slept keeps receiving every
// request to its host, each failing on the client timeout, until the process restarts.
func Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.HTTP2 = &http.HTTP2Config{SendPingTimeout: 30 * time.Second, PingTimeout: 15 * time.Second}
	return t
}
