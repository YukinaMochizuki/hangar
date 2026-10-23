package cli

// Resized is the signal that says the terminal's size changed: SIGWINCH,
// or on Windows the one watchSize raises.
var Resized = resized
