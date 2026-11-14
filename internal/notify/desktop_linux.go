// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"fmt"
	"os/exec"
)

// Serve shows the notes follow sends through notify-send, which speaks
// the freedesktop notification interface every Linux desktop and
// notification daemon (mako, dunst, swaync, KDE, GNOME) serves. harbor is
// unused here: a Linux notification has no click target.
func Serve(_ context.Context, _ string, follow func(Sender)) error {
	bin, err := exec.LookPath("notify-send")
	if err != nil {
		return fmt.Errorf("notify-send is not installed (libnotify): %w", err)
	}
	follow(notifySend(bin))
	return nil
}

type notifySend string

func (bin notifySend) Send(n Note) error {
	urgency := "normal"
	if n.Level == Error {
		urgency = "critical"
	}
	return exec.Command(string(bin), "-a", "berth", "-u", urgency, n.Title, n.Body).Run()
}
