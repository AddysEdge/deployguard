package replay

import (
	"context"
	"sync"
	"sync/atomic"
)

// ForEach calls fn(ctx, i) for each i in [0, n) with at most limit calls in
// flight. It stops starting new calls once ctx is done and returns only after
// every started call has returned, so no worker outlives it. The result is
// the number of calls that were started.
func ForEach(ctx context.Context, n, limit int, fn func(ctx context.Context, i int)) int {
	if n <= 0 {
		return 0
	}
	if limit < 1 {
		limit = 1
	}
	if limit > n {
		limit = n
	}
	var (
		next    atomic.Int64
		started atomic.Int64
		wg      sync.WaitGroup
	)
	for w := 0; w < limit; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				started.Add(1)
				fn(ctx, i)
			}
		}()
	}
	wg.Wait()
	return int(started.Load())
}
