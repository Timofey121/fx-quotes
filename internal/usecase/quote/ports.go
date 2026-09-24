package quote

import (
	"context"

	"github.com/Timofey121/fx-quotes/internal/entity"
)

// Store хранит операции обновления и последние успешные котировки.
// Переходы состояния атомарны; attempt защищает запись от прежнего обработчика.
type Store interface {
	CreateOrGet(context.Context, CreateOrGetCommand) (entity.QuoteUpdate, bool, error)
	GetUpdate(context.Context, GetUpdateQuery) (entity.QuoteUpdate, error)
	GetLatest(context.Context, GetLatestQuery) (entity.Quote, error)
	ClaimNext(context.Context, ClaimNextCommand) (entity.QuoteUpdate, error)
	Complete(context.Context, CompleteCommand) (entity.QuoteUpdate, error)
	Retry(context.Context, RetryCommand) (entity.QuoteUpdate, error)
	Fail(context.Context, FailCommand) (entity.QuoteUpdate, error)
	// Восстанавливает ограниченный пакет; непустой результат требует следующего прохода.
	RecoverExpired(context.Context, RecoverExpiredCommand) ([]entity.QuoteUpdate, error)
}
