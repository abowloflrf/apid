package server

import (
	"net/http"
	"slices"
	"strings"

	"github.com/abowloflrf/apid/config"
)

type modelsResponse struct {
	Object string      `json:"object"`
	Data   []modelInfo `json:"data"`
}

type modelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	ids := make(map[string]struct{})
	for _, rt := range s.cfg.Routes {
		if rt.Operation != config.RouteOperationInference || rt.InputProtocol != config.ProtoChat {
			continue
		}
		for _, rule := range rt.Models {
			if rule.Match == "" || strings.Contains(rule.Match, "*") {
				continue
			}
			ids[rule.Match] = struct{}{}
		}
	}

	modelIDs := make([]string, 0, len(ids))
	for id := range ids {
		modelIDs = append(modelIDs, id)
	}
	slices.Sort(modelIDs)

	resp := modelsResponse{Object: "list", Data: make([]modelInfo, 0, len(modelIDs))}
	for _, id := range modelIDs {
		resp.Data = append(resp.Data, modelInfo{
			ID:      id,
			Object:  "model",
			OwnedBy: "apid",
		})
	}
	s.writeJSON(w, resp)
}
