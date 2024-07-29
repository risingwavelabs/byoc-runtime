package wait

import (
	"context"
	"fmt"
	"testing"
)

func doSuccessOnNth(n int) func(context.Context) error {
	count := 0
	return func(context.Context) error {
		count = count + 1
		if count == n {
			return nil
		}
		return fmt.Errorf("failed on %vth try", count)
	}
}

func TestRetryWithInterval(t *testing.T) {
	tests := map[string]struct {
		do        func(context.Context) error
		retry     int
		expectErr bool
	}{
		"success wtih no retry on first try": {
			do:        doSuccessOnNth(1),
			expectErr: false,
		},
		"success on retry": {
			do:        doSuccessOnNth(2),
			retry:     2,
			expectErr: false,
		},
		"fail with no retry": {
			do:        doSuccessOnNth(2),
			expectErr: true,
		},
		"fail with retry": {
			do:        doSuccessOnNth(3),
			retry:     1,
			expectErr: true,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			test := test
			t.Parallel()
			err := RetryWithInterval(context.Background(), test.retry, 0, test.do)
			if (err != nil) != test.expectErr {
				t.Errorf("expectErr: %v, got %v", test.expectErr, err)
			}
		})
	}
}
