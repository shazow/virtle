package control

// InvalidParams reports request params that do not satisfy the method.
func InvalidParams(message string) error {
	return &RPCError{Code: ErrInvalidParams, Message: message}
}

// FailedPrecondition wraps err as an RPC failed-precondition error.
func FailedPrecondition(err error) error {
	return &RPCError{Code: ErrFailedPrecondition, Message: err.Error()}
}

// ResourceLimit wraps err as an RPC resource-limit error.
func ResourceLimit(err error) error {
	return &RPCError{Code: ErrResourceLimit, Message: err.Error()}
}
