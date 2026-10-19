// Package circuitbreaker оборачивает вызовы внешних зависимостей в circuit
// breaker: после серии неудач подряд вызовы перестают выполняться и сразу
// возвращают ошибку, не дожидаясь таймаута.
//
// Зачем это нужно поверх обычных таймаутов: когда зависимость (S3, брокер)
// лежит, каждый запрос всё равно занимает время до таймаута, а под нагрузкой
// такие запросы копятся и съедают соединения и горутины. Открытый breaker
// отвечает мгновенно и даёт зависимости время подняться.
//
// Обёртка нужна ещё и для того, чтобы наружу не протекал тип ошибки библиотеки:
// вызывающая сторона видит ErrCircuitOpen независимо от того, отверг ли запрос
// открытый breaker или лимит запросов в half-open.
package circuitbreaker

import (
	"errors"
	"log/slog"
	"time"

	// v2, а не v1: текущая мажорная версия библиотеки. Кроме генериков
	// (они убирают приведение interface{} в вызывающем коде) в ней есть
	// IsExcluded — возможность не считать ошибку ни успехом, ни неудачей.
	// Это нужно для S3: 404 на несуществующий объект — нормальный ответ, а не
	// признак деградации хранилища, и пять таких ответов не должны открывать
	// breaker.
	"github.com/sony/gobreaker/v2"
)

// ErrCircuitOpen возвращается, когда breaker открыт (или исчерпал лимит
// запросов в half-open) и вызов зависимости не выполнялся.
//
// Это не ошибка зависимости, а сигнал «мы даже не пробовали»: вызывающая
// сторона может отдать клиенту 503, не дожидаясь таймаута.
var ErrCircuitOpen = errors.New("circuit breaker is open")

const (
	// maxRequests — сколько запросов пропускается в half-open, чтобы проверить,
	// поднялась ли зависимость.
	maxRequests = 3
	// openTimeout — сколько breaker остаётся открытым до перехода в half-open.
	openTimeout = 30 * time.Second
	// failuresToTrip — сколько неудач подряд открывают breaker.
	failuresToTrip = 5
)

// Breaker — circuit breaker для одной внешней зависимости.
//
// Параметр типа зафиксирован как any: один breaker обслуживает и операции со
// значением (Upload возвращает UploadInfo), и операции без него (Delete).
// Состояние у них должно быть общим — деградирует зависимость, а не отдельный
// вызов, поэтому отдельные breaker'ы на каждый тип ответа были бы ошибкой.
type Breaker = gobreaker.CircuitBreaker[any]

// Option настраивает breaker до его создания.
type Option func(*gobreaker.Settings)

// New создаёт breaker с настройками по умолчанию: 5 неудач подряд открывают
// его на 30 секунд, затем half-open пропускает 3 запроса, и если все три
// успешны — breaker закрывается.
//
// Interval равен нулю: счётчики не сбрасываются по времени, поэтому «подряд»
// означает именно подряд — серия неудач не размывается по окну. Сбрасывает
// счётчик только успешный вызов.
func New(name string, opts ...Option) *Breaker {
	settings := gobreaker.Settings{
		Name:        name,
		MaxRequests: maxRequests,
		Interval:    0,
		Timeout:     openTimeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= failuresToTrip
		},
	}

	for _, opt := range opts {
		opt(&settings)
	}

	return gobreaker.NewCircuitBreaker[any](settings)
}

// Execute выполняет fn через breaker и возвращает её результат.
//
// Если breaker открыт, fn не вызывается, а возвращается ErrCircuitOpen.
func Execute[T any](cb *Breaker, fn func() (T, error)) (T, error) {
	// Результат пишется в захваченную переменную, а не возвращается из
	// замыкания: значение всё равно пришлось бы приводить из any, а nil-значение
	// интерфейсного типа при таком приведении терялось бы. Здесь же значение
	// остаётся в своей типизированной переменной.
	var result T

	_, err := cb.Execute(func() (any, error) {
		v, callErr := fn()
		if callErr == nil {
			result = v
		}
		// Значение наружу не отдаём: breaker принимает решение только по
		// ошибке, и лишнее приведение типов ему не нужно.
		return nil, callErr
	})
	if err != nil {
		var zero T
		return zero, mapError(err)
	}

	return result, nil
}

// ExecuteNoResult выполняет fn через breaker, когда результат не нужен
// (удаление, публикация).
func ExecuteNoResult(cb *Breaker, fn func() error) error {
	_, err := Execute(cb, func() (struct{}, error) {
		return struct{}{}, fn()
	})
	return err
}

// WithTimeout задаёт, сколько breaker остаётся открытым до перехода в half-open.
// Нужен тестам: ждать 30 секунд в них нельзя.
func WithTimeout(d time.Duration) Option {
	return func(s *gobreaker.Settings) {
		s.Timeout = d
	}
}

// WithIsExcluded задаёт ошибки, которые не считаются ни успехом, ни неудачей.
//
// Для S3 это 404: объект могли удалить, и это штатный ответ, а не признак
// недоступности хранилища. Отличается от «считать успехом» тем, что такой ответ
// не сбрасывает счётчик неудач подряд — серия реальных сбоев не маскируется
// случайным 404 в середине.
func WithIsExcluded(fn func(error) bool) Option {
	return func(s *gobreaker.Settings) {
		s.IsExcluded = fn
	}
}

// LogStateChanges логирует переходы состояний через slog: открытие — Warn (это
// деградация зависимости, которую нужно видеть в логах и алертах), остальные
// переходы — Info. Без этого открытый breaker выглядит как беспричинная серия
// ошибок «circuit breaker is open».
func LogStateChanges(log *slog.Logger) Option {
	if log == nil {
		log = slog.Default()
	}

	return func(s *gobreaker.Settings) {
		s.OnStateChange = func(name string, from, to gobreaker.State) {
			if to == gobreaker.StateOpen {
				log.Warn("circuit breaker opened",
					"breaker", name,
					"from", from.String(),
				)
				return
			}
			log.Info("circuit breaker state changed",
				"breaker", name,
				"from", from.String(),
				"to", to.String(),
			)
		}
	}
}

// mapError приводит ошибки библиотеки к ErrCircuitOpen.
//
// ErrTooManyRequests (лимит запросов в half-open) переводится в то же значение:
// для вызывающей стороны это тоже «зависимость сейчас не вызывается», а тип
// ошибки gobreaker наружу протекать не должен.
func mapError(err error) error {
	if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
		return ErrCircuitOpen
	}
	return err
}
