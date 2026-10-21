module minimax2api

go 1.24

// Pinned so an ordinary `go build` (including Minimax2api.bat pack) cannot
// silently fall back to an older stdlib: 1.27.2 fixed the net/http and
// crypto/tls advisories, 1.27.3 fixed crypto/x509. See govulncheck output.
toolchain go1.27.3
