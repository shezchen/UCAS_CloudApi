package streams

// Stream represents a generic stream interface
// The caller should check the Err() method to ensure there's no error.
//
// Contract: after each successful Next, call Current exactly once. Some
// implementations advance internal state inside Current (making repeated
// calls return different values), while others return the same value until
// the next Next; callers must not rely on either behavior. Streams are not
// safe for concurrent use.
type Stream[T any] interface {
	// Next indicate if there's a next item.
	Next() bool
	// Current returns the current event
	Current() T
	// Err returns any error that occurred
	Err() error
	// Close closes the stream
	Close() error
}
