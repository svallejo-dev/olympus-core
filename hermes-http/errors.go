package hermes

// otelhttp reports the concrete type of a transport error as the error.type metric attribute,
// so each failure hermes produces has its own type instead of a shared *fmt.wrapError.
type wrapped struct{ error }

func (w wrapped) Unwrap() error { return w.error }

type (
	circuitOpenError    struct{ wrapped }
	attemptTimeoutError struct{ wrapped }
	rateLimitError      struct{ wrapped }
	concurrencyError    struct{ wrapped }
	retryWaitError      struct{ wrapped }
)
