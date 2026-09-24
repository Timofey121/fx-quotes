package quote

import (
	"errors"
)

// MaxIdempotencyKeyBytes ограничивает размер сохраняемого ключа и записи в индексе БД.
const MaxIdempotencyKeyBytes = 128

var (
	ErrNotFound              = errors.New("quote update not found")
	ErrCapacityExceeded      = errors.New("quote update capacity exceeded")
	ErrNoWork                = errors.New("no quote update ready for processing")
	ErrIdempotencyConflict   = errors.New("idempotency key conflicts with pair")
	ErrInvalidIdempotencyKey = errors.New("idempotency key exceeds maximum byte length")
	ErrLostOwnership         = errors.New("quote update ownership lost")
)
