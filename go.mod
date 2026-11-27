module github.com/kfet/zulip-acp

go 1.25.0

require (
	github.com/coder/acp-go-sdk v0.13.5
	github.com/coder/websocket v1.8.15
	github.com/kfet/acp-kit v0.52.3
	github.com/kfet/distkit v0.1.10
	golang.org/x/image v0.31.0
	golang.org/x/term v0.40.0
)

require (
	github.com/kfet/covgate v0.1.2 // indirect
	golang.org/x/sys v0.41.0 // indirect
)

tool github.com/kfet/covgate/cmd/covgate
