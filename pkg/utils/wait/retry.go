package wait

import (
	"context"
	"time"

	"github.com/risingwavelabs/eris"
)

// RetryWithInterval will rety the do function at most `time` times until the
// function returns no error if the initial execution fails.
func RetryWithInterval(ctx context.Context, times int, interval time.Duration, do func(context.Context) error) error {
	times = max(times, 0)
	var err error
	for i := 0; i < times; i++ {
		err = do(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return eris.Wrapf(err, "context got cancelled %v", ctx.Err())
		case <-time.After(interval):
			continue
		}
	}
	return do(ctx)
}
