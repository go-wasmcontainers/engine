package api

import "context"

// listing is the shape other modules declare and satisfy with *Client
// (go-wasmcontainers/dashboard's engine.Engine embeds these): changing one
// of these signatures, even by adding a variadic parameter, breaks them.
// v0.18.0 did, and dashboard v0.6.0 stopped compiling against it.
type listing interface {
	Containers(ctx context.Context, all bool) ([]ContainerSummary, error)
	Networks(ctx context.Context) ([]Network, error)
	Volumes(ctx context.Context) ([]Volume, error)
	Images(ctx context.Context) ([]ImageSummary, error)
}

var _ listing = (*Client)(nil)
