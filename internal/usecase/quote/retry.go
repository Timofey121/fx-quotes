package quote

import (
	"encoding/binary"
	"hash/fnv"
	"time"
)

// RetryPolicy выбирает задержку в пределах [backoff/2, backoff] после ограничения
// экспоненциального роста. Подсказка провайдера задаёт нижнюю границу в пределах
// того же максимума. Нулевые поля заменяются на одну секунду и одну минуту.
type RetryPolicy struct {
	Initial time.Duration
	Maximum time.Duration
}

func (policy RetryPolicy) Delay(updateID string, attempt int64, providerHint time.Duration) time.Duration {
	initial, maximum := policy.Initial, policy.Maximum
	if initial <= 0 {
		initial = time.Second
	}
	if maximum <= 0 {
		maximum = time.Minute
	}
	backoff := min(initial, maximum)
	for n := int64(1); n < attempt && backoff < maximum; n++ {
		if backoff > maximum/2 {
			backoff = maximum
		} else {
			backoff *= 2
		}
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(updateID))
	var token [8]byte
	binary.LittleEndian.PutUint64(token[:], uint64(attempt))
	_, _ = hash.Write(token[:])
	floor := max(time.Nanosecond, backoff/2)
	delay := floor + time.Duration(hash.Sum64()%uint64(backoff-floor+1))
	return min(maximum, max(delay, providerHint))
}
