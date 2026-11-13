// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package server

// OpQueue. The operator bumps, re-ranks or drops one waiter; the reply names
// the change, and the broker journals it as an event.

import (
	"log/slog"

	"github.com/T-Crypt/berth/internal/broker"
	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/proto"
)

func queue(conn *proto.Conn, log *slog.Logger, req proto.Msg, n *node.Node) {
	if req.Lease == "" {
		reply(conn, log, proto.Msg{Op: proto.OpRefused, Reason: "lease ID is required"})
		return
	}
	detail, err := n.Queue(req.Lease, broker.QueueAction(req.Action), req.Priority)
	if err != nil {
		reply(conn, log, proto.Msg{Op: proto.OpRefused, Reason: err.Error()})
		return
	}
	reply(conn, log, proto.Msg{Op: proto.OpQueue, Lease: req.Lease, Reason: detail})
}
