package cli

// Terminal is the local terminal an exec with -t runs on.
type Terminal interface {
	// MakeRaw passes every key through as it is typed, Ctrl-C included,
	// and echoes nothing; restore puts the terminal back as it was.
	MakeRaw() (restore func(), err error)
	// Size is the terminal's rows and columns.
	Size() (rows, cols uint16, err error)
}
