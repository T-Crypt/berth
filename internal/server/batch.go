// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package server

// OpBatch. A batch starts, renews or ends on every card; the reply echoes
// the op. Starting one drains nothing by itself: the engine's lease keeps
// the card, and its waiting lease takes it as a switch does.

import (
	"log/slog"
	"time"

	"github.com/T-Crypt/berth/internal/node"
	"github.com/T-Crypt/berth/internal/proto"
)

func batch(conn *proto.Conn, log *slog.Logger, req proto.Msg, n *node.Node) {
	if req.Engine == "" {
		reply(conn, log, proto.Msg{Op: proto.OpRefused, Reason: "engine name is required"})
		return
	}
	if req.End {
		if !n.EndBatch(req.Engine) {
			reply(conn, log, proto.Msg{Op: proto.OpRefused, Reason: "no batch for " + req.Engine})
			return
		}
		reply(conn, log, proto.Msg{Op: proto.OpBatch, Engine: req.Engine, End: true})
		return
	}
	if req.Grace < 0 || req.Max < 0 {
		reply(conn, log, proto.Msg{Op: proto.OpRefused, Reason: "grace and max must not be negative"})
		return
	}
	n.Batch(req.Engine, seconds(req.Grace), seconds(req.Max))
	reply(conn, log, proto.Msg{Op: proto.OpBatch, Engine: req.Engine})
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
