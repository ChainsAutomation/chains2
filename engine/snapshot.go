package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	chains "chains-automation"
	"chains-automation/state"
)

const snapshotChunkSize = 512 << 10
const maxSnapshotChunks = 128

type snapshotChunk struct {
	Index int    `json:"index"`
	Count int    `json:"count"`
	Data  []byte `json:"data"`
}

func (e *Engine) sendSnapshot(ctx context.Context, request chains.Message) error {
	if e.snapshotSlots == nil {
		return e.writeSnapshot(ctx, request)
	}
	select {
	case e.snapshotSlots <- struct{}{}:
	default:
		e.log.Debug("snapshot sender busy", "target", request.Source)
		return nil
	}
	e.snapshotWG.Add(1)
	go func() {
		defer e.snapshotWG.Done()
		defer func() { <-e.snapshotSlots }()
		if err := e.writeSnapshot(ctx, request); err != nil && ctx.Err() == nil {
			e.log.Warn("send state snapshot", "target", request.Source, "error", err)
		}
	}()
	return nil
}

func (e *Engine) writeSnapshot(ctx context.Context, request chains.Message) error {
	raw, err := json.Marshal(e.view.Snapshot())
	if err != nil {
		return err
	}
	count := (len(raw) + snapshotChunkSize - 1) / snapshotChunkSize
	if count > maxSnapshotChunks {
		return fmt.Errorf("state snapshot exceeds %d chunks", maxSnapshotChunks)
	}
	for i := 0; i < count; i++ {
		part := raw[i*snapshotChunkSize : min(len(raw), (i+1)*snapshotChunkSize)]
		m, err := chains.New(chains.SnapshotResponse, e.node.Source, snapshotChunk{Index: i, Count: count, Data: part})
		if err != nil {
			return err
		}
		m.Target = request.Source
		m.CausedBy(request)
		if e.local != nil {
			e.mirror(m)
		} else if err = e.node.Send(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) receiveSnapshot(ctx context.Context, m chains.Message) error {
	var chunk snapshotChunk
	if err := json.Unmarshal(m.Data, &chunk); err != nil {
		return err
	}
	if chunk.Count < 1 || chunk.Count > maxSnapshotChunks || chunk.Index < 0 || chunk.Index >= chunk.Count ||
		len(chunk.Data) == 0 || len(chunk.Data) > snapshotChunkSize {
		return errors.New("invalid state snapshot chunk")
	}
	if e.snapshotParts == nil {
		e.snapshotParts = make([][]byte, chunk.Count)
	}
	if len(e.snapshotParts) != chunk.Count {
		return errors.New("inconsistent state snapshot chunks")
	}
	e.snapshotParts[chunk.Index] = chunk.Data
	for _, part := range e.snapshotParts {
		if part == nil {
			return nil
		}
	}
	var s state.Snapshot
	raw := bytes.Join(e.snapshotParts, nil)
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	if s.Epoch == "" || s.Devices == nil || s.Services == nil {
		return errors.New("invalid state snapshot")
	}
	e.snapshotParts = nil
	e.snapshotPending = ""
	epoch, rev := e.view.Position()
	if epoch != s.Epoch || rev <= s.Revision {
		e.view.Replace(s)
	}
	sort.Slice(e.queuedUpdates, func(i, j int) bool { return e.queuedUpdates[i].Revision < e.queuedUpdates[j].Revision })
	incomplete := false
	for _, update := range e.queuedUpdates {
		if update.Epoch == s.Epoch && !e.view.Apply(update) {
			incomplete = true
			break
		}
	}
	e.ready.Store(!incomplete)
	if incomplete {
		e.requestSnapshot(ctx)
	} else {
		e.queuedUpdates = nil
		e.flushEvents()
	}
	return nil
}
