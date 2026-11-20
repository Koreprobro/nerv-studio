// SPDX-License-Identifier: AGPL-3.0-or-later
//
// nerv-core — omni-channel AI studio core: a thin MCP orchestrator of
// system-installed generation tools, a GPU scheduler and an artifact server.
// Copyright (C) 2026 Koreprobro
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful, but
// WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package mcp

import (
	"encoding/json"
	"net/http"
)

// handleWaveStop is the human red button outside MCP (SPECS §6.6):
// POST /wave/stop cancels the live wave — the scheduler kills its process
// groups, the wave finishes not-ok and its queue files receive the wave
// block as usual (the drain then reload still guards the next chat
// start). Without ?wave=<id> the newest unfinished wave is stopped;
// stopping a finished wave — or pressing with no waves at all — answers
// idempotently with stopped:false. The GUI needs the button because in
// XOR mode the LLM sleeps through the wave and cannot type "stop".
func (s *Server) handleWaveStop(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	wv := s.findStopWave(r.URL.Query().Get("wave"))
	if wv == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"stopped": false, "reason": "no active wave"})
		return
	}
	wv.mu.Lock()
	finished := !wv.Ended.IsZero()
	wv.mu.Unlock()
	if !finished {
		wv.cancel()
	}
	resp := map[string]any{"stopped": !finished, "wave": wv.ID}
	if finished {
		resp["reason"] = "already finished"
	} else {
		resp["status"] = "cancelling"
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// findStopWave resolves a /wave/stop target: an exact id, or — with none
// given — the newest wave still queued or running.
func (s *Server) findStopWave(id string) *wave {
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	for i := len(s.ledger) - 1; i >= 0; i-- {
		w := s.ledger[i]
		if id != "" {
			if w.ID == id {
				return w
			}
			continue
		}
		w.mu.Lock()
		finished := !w.Ended.IsZero()
		w.mu.Unlock()
		if !finished {
			return w
		}
	}
	return nil
}
