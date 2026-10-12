package registry

import "io"

// WithOpen has the driver open its cached blobs with open.
func WithOpen(cfg Config, open func(name string) (io.ReadCloser, error)) Config {
	cfg.open = open
	return cfg
}
