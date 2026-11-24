package infra

import (
	"context"
	"fmt"
	"sync"
)

// AsyncResult 异步任务结果
type AsyncResult[T any] struct {
	Value T
	Err   error
}

// RunAsync 在 goroutine 中异步执行函数，返回 channel 接收结果
// 函数执行完毕后 channel 会自动关闭
func RunAsync[T any](fn func() (T, error)) <-chan AsyncResult[T] {
	ch := make(chan AsyncResult[T], 1)
	go func() {
		defer close(ch)
		val, err := fn()
		ch <- AsyncResult[T]{Value: val, Err: err}
	}()
	return ch
}

// RunAsyncVoid 异步执行无返回值的函数，返回 error channel
func RunAsyncVoid(fn func() error) <-chan error {
	ch := make(chan error, 1)
	go func() {
		defer close(ch)
		ch <- fn()
	}()
	return ch
}

// RunWithTimeout 带超时的异步执行
// 如果 ctx 取消或超时，返回 context 错误
func RunWithTimeout[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var zero T

	// 首先检查 ctx 是否已结束
	if err := ctx.Err(); err != nil {
		return zero, err
	}

	resultCh := make(chan AsyncResult[T], 1)
	go func() {
		val, err := fn()
		resultCh <- AsyncResult[T]{Value: val, Err: err}
	}()

	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case result := <-resultCh:
		return result.Value, result.Err
	}
}

// RunWithTimeoutVoid 带超时的异步执行（无返回值版本）
func RunWithTimeoutVoid(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- fn()
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// ParallelResults 并行执行多个函数，返回聚合结果
// 结果顺序与函数顺序一致
func ParallelResults[T any](ctx context.Context, fns ...func() (T, error)) ([]T, []error) {
	var wg sync.WaitGroup
	results := make([]T, len(fns))
	errs := make([]error, len(fns))

	for i, fn := range fns {
		wg.Add(1)
		go func(idx int, f func() (T, error)) {
			defer wg.Done()
			// 在启动 goroutine 前先检查 ctx
			if ctx.Err() != nil {
				errs[idx] = ctx.Err()
				return
			}
			val, err := f()
			results[idx] = val
			errs[idx] = err
		}(i, fn)
	}

	wg.Wait()
	return results, errs
}

// 确保 fmt 在未使用时不会报错（仅在编译不含 fmt 的构建标签时保护）
var _ = fmt.Sprintf
