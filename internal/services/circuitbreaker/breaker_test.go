package circuitbreaker

import (
	"errors"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errDependency — ошибка «зависимость недоступна»: именно её breaker считает
// неудачей и по серии таких ошибок открывается.
var errDependency = errors.New("dependency is unavailable")

// fail — вызов, который всегда падает.
func fail() error { return errDependency }

// ok — вызов, который всегда успешен.
func ok() error { return nil }

func TestExecute_ReturnsValue(t *testing.T) {
	cb := New("test")

	got, err := Do(cb, func() (string, error) { return "payload", nil })

	require.NoError(t, err)
	assert.Equal(t, "payload", got)
}

// Проверка, что nil-значение не подменяется нулевым значением «настоящего»
// типа: результат пишется в типизированную переменную, а не приводится из any.
func TestExecute_PreservesNilValue(t *testing.T) {
	cb := New("test")

	got, err := Do(cb, func() (*int, error) { return nil, nil })

	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestExecute_ReturnsDependencyError(t *testing.T) {
	cb := New("test")

	_, err := Do(cb, func() (struct{}, error) { return struct{}{}, errDependency })

	require.ErrorIs(t, err, errDependency)
}

func TestBreaker_OpensAfterConsecutiveFailures(t *testing.T) {
	cb := New("test")

	// Первые failuresToTrip вызовов выполняются и падают настоящей ошибкой
	// зависимости: breaker открывается только на последней из них.
	for i := 0; i < failuresToTrip; i++ {
		err := cb.ExecuteNoResult(fail)
		require.ErrorIs(t, err, errDependency, "вызов %d должен дойти до зависимости", i)
	}

	// Дальше вызовы не выполняются вообще.
	err := cb.ExecuteNoResult(func() error {
		t.Fatal("зависимость не должна вызываться при открытом breaker")
		return nil
	})

	require.ErrorIs(t, err, ErrCircuitOpen)
	assert.Equal(t, StateOpen, cb.State())
}

// Успешный вызов в середине серии сбрасывает счётчик «подряд»: до открытия
// breaker дело не доходит.
func TestBreaker_SuccessResetsFailureStreak(t *testing.T) {
	cb := New("test")

	for i := 0; i < failuresToTrip-1; i++ {
		require.ErrorIs(t, cb.ExecuteNoResult(fail), errDependency)
	}
	require.NoError(t, cb.ExecuteNoResult(ok))
	for i := 0; i < failuresToTrip-1; i++ {
		require.ErrorIs(t, cb.ExecuteNoResult(fail), errDependency)
	}

	assert.Equal(t, StateClosed, cb.State())
}

// Half-open: после таймаута breaker пропускает maxRequests пробных вызовов, и
// если все они успешны — закрывается.
func TestBreaker_HalfOpenClosesAfterSuccesses(t *testing.T) {
	// Таймаут в миллисекунду вместо боевых 30 секунд: тест не должен ждать.
	cb := New("test", WithTimeout(time.Millisecond))

	for i := 0; i < failuresToTrip; i++ {
		require.ErrorIs(t, cb.ExecuteNoResult(fail), errDependency)
	}
	require.Equal(t, StateOpen, cb.State())

	time.Sleep(5 * time.Millisecond)

	for i := 0; i < maxRequests; i++ {
		require.NoError(t, cb.ExecuteNoResult(ok), "пробный вызов %d в half-open", i)
	}

	assert.Equal(t, StateClosed, cb.State())
}

// Ошибка из IsExcluded не считается ни успехом, ни неудачей: сколько её ни
// возвращай, breaker остаётся закрытым.
func TestBreaker_ExcludedErrorDoesNotTrip(t *testing.T) {
	errNotFound := errors.New("not found")
	cb := New("test", WithIsExcluded(func(err error) bool { return errors.Is(err, errNotFound) }))

	for i := 0; i < failuresToTrip*2; i++ {
		require.ErrorIs(t, cb.ExecuteNoResult(func() error { return errNotFound }), errNotFound)
	}

	assert.Equal(t, StateClosed, cb.State())
}

// Исключённая ошибка не сбрасывает и счётчик реальных неудач: серия сбоев
// продолжает копиться, даже если в середине пришёл 404.
func TestBreaker_ExcludedErrorKeepsFailureStreak(t *testing.T) {
	errNotFound := errors.New("not found")
	cb := New("test", WithIsExcluded(func(err error) bool { return errors.Is(err, errNotFound) }))

	for i := 0; i < failuresToTrip-1; i++ {
		require.ErrorIs(t, cb.ExecuteNoResult(fail), errDependency)
	}
	require.ErrorIs(t, cb.ExecuteNoResult(func() error { return errNotFound }), errNotFound)
	// Последняя неудача в серии доводит счётчик до порога и открывает breaker.
	require.ErrorIs(t, cb.ExecuteNoResult(fail), errDependency)

	require.ErrorIs(t, cb.ExecuteNoResult(ok), ErrCircuitOpen)
	assert.Equal(t, StateOpen, cb.State())
}

func TestMapError_PassesThroughForeignErrors(t *testing.T) {
	assert.ErrorIs(t, mapError(errDependency), errDependency)
	assert.ErrorIs(t, mapError(gobreaker.ErrOpenState), ErrCircuitOpen)
	assert.ErrorIs(t, mapError(gobreaker.ErrTooManyRequests), ErrCircuitOpen)
}
