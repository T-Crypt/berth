// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package server

// OpCordon and OpUncordon. The named cards stop or resume new grants; the
// reply names them. A cordon drains nothing.

import (
	"log/slog"

	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/proto"
)

func cordon(conn *proto.Conn, log *slog.Logger, req proto.Msg, n *node.Node) {
	if req.Card == "" {
		reply(conn, log, proto.Msg{Op: proto.OpRefused, Reason: "name the card to " + req.Op})
		return
	}
	cards, err := n.Cordon(req.Card, req.Op == proto.OpCordon)
	if err != nil {
		reply(conn, log, proto.Msg{Op: proto.OpRefused, Reason: err.Error()})
		return
	}
	reply(conn, log, proto.Msg{Op: req.Op, Cards: cards})
}
