// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"

	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/proto"
)

// yield answers OpYield. Ending a yield also resumes the engine when a drain
// parked it during the yield, so the job that yielded gets the card back by
// the same queue a woken engine takes.
func yield(conn *proto.Conn, log *slog.Logger, req proto.Msg, n *node.Node) {
	if req.Engine == "" || req.Yield < 0 {
		reply(conn, log, proto.Msg{Op: proto.OpRefused, Reason: "an engine name and a yield of 0 or more seconds are required"})
		return
	}
	ids, err := n.Yield(req.Engine, seconds(req.Yield))
	if err != nil {
		reply(conn, log, proto.Msg{Op: proto.OpRefused, Reason: err.Error()})
		return
	}
	// Resume says the end queued a parked engine: the caller waits for its
	// lease.resumed before the job may load onto the card again.
	resumed := req.Yield == 0 && n.Resume(req.Engine, false) == nil
	reply(conn, log, proto.Msg{Op: proto.OpYield, Engine: req.Engine, Yield: req.Yield, Leases: ids, Resume: resumed})
}
