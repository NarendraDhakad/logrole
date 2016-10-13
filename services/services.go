package services

import "time"

// Shorter returns a potentially obfuscated, shorter version of the given
// string.
func Shorter(s string) (string, error) {
	// TODO - figure out a compression scheme for the next page url. Gzip/flate
	// both return a longer string. At the very least we could represent the
	// account sid and page token using fewer vars.
	return s, nil
}

// Unshorter reverses the effects of a string shortened with Shorter.
func Unshorter(compressed string) string {
	// TODO
	return compressed
}

// Duration returns a friendly duration (with the insignificant bits rounded
// off).
func Duration(d time.Duration) string {
	d2 := (d / (100 * time.Microsecond)) * (100 * time.Microsecond)
	return d2.String()
}