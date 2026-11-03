// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package server

// OpResume: any client (the gateway, berth resume) puts a parked engine
// back in the queue at its own priority. The resuming connection gets one
// reply; the runner's own connection hears OpGranted{Resume: true} when the
// grant lands.

import (
	"log/slog"

	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/proto"
)

func resume(conn *proto.Conn, log *slog.Logger, req proto.Msg, n *node.Node) {
	err := n.Resume(req.Engine, req.Switched)
	switch {
	case err == nil:
		reply(conn, log, proto.Msg{Op: proto.OpGranted, Engine: req.Engine, Reason: "queued at its own priority"})
	case req.Engine == "":
		reply(conn, log, proto.Msg{Op: proto.OpRefused, Reason: "engine name is required"})
	default:
		reply(conn, log, proto.Msg{Op: proto.OpRefused, Reason: err.Error()})
	}
}

// parkMode answers OpParkMode: with ParkOff set it flips the switch (off
// evicts what is parked), without it it only reports.
func parkMode(conn *proto.Conn, log *slog.Logger, req proto.Msg, n *node.Node) {
	ids := []string{}
	if req.ParkOff != nil {
		ids = n.SetParkOff(*req.ParkOff)
	}
	off := n.ParkOff()
	reply(conn, log, proto.Msg{Op: proto.OpParkMode, ParkOff: &off, Leases: ids})
}

// switched answers OpSwitch: the engine's queued lease is now a person's
// switch.
func switched(conn *proto.Conn, log *slog.Logger, req proto.Msg, n *node.Node) {
	if err := n.Switch(req.Engine); err != nil {
		reply(conn, log, proto.Msg{Op: proto.OpRefused, Reason: err.Error()})
		return
	}
	reply(conn, log, proto.Msg{Op: proto.OpGranted, Engine: req.Engine, Reason: "switched"})
}
