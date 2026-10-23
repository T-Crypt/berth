// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !windows

package notify

import (
	"context"
	"errors"
	"runtime"
)

// Serve has no notification service on this system yet.
func Serve(context.Context, string, func(Sender)) error {
	return errors.New("desktop notifications are not supported on " + runtime.GOOS + " yet")
}
