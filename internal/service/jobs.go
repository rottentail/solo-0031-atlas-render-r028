package service

import (
	"atlas-render/internal/model"
	"atlas-render/internal/raster"
	"atlas-render/internal/scene"
	"fmt"
	"time"
)

func (s *Service) Submit(snapshotID string, scale int, key string) (model.Job, error) {
	var out model.Job
	if !scene.ValidID(key) {
		return out, model.Invalid("idempotency key required (1..80 identifier characters)")
	}
	fingerprint := model.CanonicalDigest([]any{snapshotID, scale})
	e := s.Store.Update(func(st *model.State) error {
		if r, ok := st.Receipts[key]; ok {
			if r.Fingerprint != fingerprint {
				return model.Conflict("idempotency key used for different request")
			}
			out = st.Jobs[r.JobID]
			return nil
		}
		snap, ok := st.Snapshots[snapshotID]
		if !ok {
			return model.Missing("snapshot not found")
		}
		if snap.State != "active" {
			return model.Conflict("snapshot is revoked")
		}
		if _, e := raster.Compile(snap, scale); e != nil {
			return e
		}
		pending := 0
		for _, j := range st.Jobs {
			if j.State == "queued" || j.State == "running" {
				pending++
			}
		}
		if pending >= 64 {
			return model.Busy("render queue is full")
		}
		now := time.Now().UTC()
		out = model.Job{ID: model.ID("job", st.Sequence+1), SnapshotID: snapshotID, State: "queued", Scale: scale, Attempt: 1, Lineage: model.RootLineage(model.ID("job", st.Sequence+1)), CreatedAt: now, UpdatedAt: now, Payload: snap}
		st.Jobs[out.ID] = out
		st.Receipts[key] = model.Receipt{Key: key, Fingerprint: fingerprint, JobID: out.ID}
		st.Emit("job.queued", out.ID)
		return nil
	})
	return out, e
}
func (s *Service) Cancel(id string) (model.Job, error) {
	var out model.Job
	e := s.Store.Update(func(st *model.State) error {
		j, ok := st.Jobs[id]
		if !ok {
			return model.Missing("job not found")
		}
		if j.State == "cancelled" {
			out = j
			return nil
		}
		if j.State != "queued" && j.State != "running" {
			return model.Conflict("job has already finished")
		}
		j.State = "cancelled"
		j.UpdatedAt = time.Now().UTC()
		st.Jobs[id] = j
		st.Emit("job.cancelled", id)
		out = j
		return nil
	})
	return out, e
}
func (s *Service) Retry(id string) (model.Job, error) {
	var out model.Job
	e := s.Store.Update(func(st *model.State) error {
		j, ok := st.Jobs[id]
		if !ok {
			return model.Missing("job not found")
		}
		if j.State != "failed" && j.State != "cancelled" {
			return model.Conflict("only failed or cancelled jobs can retry")
		}
		n := 0
		for _, v := range st.Jobs {
			if v.State == "queued" || v.State == "running" {
				n++
			}
		}
		if n >= 64 {
			return model.Busy("render queue is full")
		}
		j.State = "queued"
		j.Error = ""
		j.Attempt++
		j.Lineage.Source = "retry"
		j.Lineage.Mode = "frozen"
		j.Lineage.ParentID = j.ID
		j.UpdatedAt = time.Now().UTC()
		st.Jobs[id] = j
		st.Emit("job.retried", id)
		out = j
		return nil
	})
	return out, e
}

// Rerun starts a fresh execution after a failed or cancelled job.
//
// mode "frozen" re-runs the exact snapshot captured for the source job, so the
// new attempt uses the same fixed input even when the scene changed afterwards.
// mode "current" freezes the scene revision named by expectedRevision inside
// this transaction and renders that new version; the caller must name the
// revision explicitly instead of silently inheriting whatever is latest.
//
// The source terminal job is never mutated, so cancellation and completion of
// the old attempt cannot both produce results. The idempotency key makes
// repeated clicks and process restarts replay one single execution promise.
func (s *Service) Rerun(id, mode string, expected int, key string) (model.Job, error) {
	var out model.Job
	if !scene.ValidID(key) {
		return out, model.Invalid("idempotency key required (1..80 identifier characters)")
	}
	if mode != "frozen" && mode != "current" {
		return out, model.Invalid(`mode must be "frozen" or "current"`)
	}
	if mode == "current" && expected < 1 {
		return out, model.Invalid("expectedRevision is required for mode \"current\"")
	}
	fingerprint := model.CanonicalDigest([]any{"rerun", id, mode, expected})
	e := s.Store.Update(func(st *model.State) error {
		if r, ok := st.Receipts[key]; ok {
			if r.Fingerprint != fingerprint {
				return model.Conflict("idempotency key used for different request")
			}
			out = st.Jobs[r.JobID]
			return nil
		}
		src, ok := st.Jobs[id]
		if !ok {
			return model.Missing("job not found")
		}
		if src.State != "failed" && src.State != "cancelled" {
			return model.Conflict("only failed or cancelled jobs can be re-run")
		}

		var snap model.Snapshot
		switch mode {
		case "frozen":
			snap, ok = st.Snapshots[src.SnapshotID]
			if !ok {
				return model.Missing("frozen snapshot not found")
			}
			if snap.State != "active" {
				return model.Conflict("frozen snapshot is revoked")
			}
		case "current":
			sc, ok := st.Scenes[src.Payload.SceneID]
			if !ok {
				return model.Missing("source scene not found")
			}
			if sc.Revision != expected {
				return model.Conflict("scene revision changed")
			}
			if sc.State != "active" {
				return model.Conflict("scene is archived")
			}
			assets := map[string]model.Asset{}
			for _, ref := range scene.References(sc.Document) {
				a, ok := st.Assets[ref]
				if !ok || a.State != "available" {
					return model.Conflict("scene references unavailable asset")
				}
				assets[ref] = a
			}
			snap = model.Snapshot{ID: model.ID("snap", st.Sequence+1), SceneID: sc.ID, Revision: sc.Revision, State: "active", Document: sc.Document, Assets: assets, CreatedAt: time.Now().UTC()}
			snap.Digest = model.SnapshotDigest(snap)
			st.Snapshots[snap.ID] = snap
			st.Emit("snapshot.frozen", snap.ID)
		}

		if _, e := raster.Compile(snap, src.Scale); e != nil {
			return e
		}
		pending := 0
		for _, j := range st.Jobs {
			if j.State == "queued" || j.State == "running" {
				pending++
			}
		}
		if pending >= 64 {
			return model.Busy("render queue is full")
		}
		root := src.Lineage.RootID
		if root == "" {
			root = src.ID
		}
		now := time.Now().UTC()
		out = model.Job{
			ID:         model.ID("job", st.Sequence+1),
			SnapshotID: snap.ID,
			State:      "queued",
			Scale:      src.Scale,
			Attempt:    1,
			Lineage: model.Lineage{
				RootID:   root,
				ParentID: src.ID,
				Depth:    src.Lineage.Depth + 1,
				Source:   "rerun",
				Mode:     mode,
			},
			CreatedAt: now,
			UpdatedAt: now,
			Payload:   snap,
		}
		st.Jobs[out.ID] = out
		st.Receipts[key] = model.Receipt{Key: key, Fingerprint: fingerprint, JobID: out.ID}
		st.Emit("job.rerun", out.ID)
		return nil
	})
	return out, e
}
func (s *Service) Purge(id string) error {
	return s.Store.Update(func(st *model.State) error {
		a, ok := st.Artifacts[id]
		if !ok {
			return model.Missing("artifact not found")
		}
		if a.State == "purged" {
			return nil
		}
		a.State = "purged"
		a.PNG = nil
		st.Artifacts[id] = a
		st.Emit("artifact.purged", id)
		return nil
	})
}
func (s *Service) Artifact(id string) (model.Artifact, error) {
	a, ok := s.Store.Read().Artifacts[id]
	if !ok {
		return a, model.Missing("artifact not found")
	}
	if a.State == "purged" {
		return a, model.Gone(fmt.Sprintf("artifact %s was purged", id))
	}
	return a, nil
}
