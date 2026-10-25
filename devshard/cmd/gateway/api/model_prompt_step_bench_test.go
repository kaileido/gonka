package api

import (
	"devshard/cmd/gateway/config"
	"devshard/cmd/gateway/filters"
)

func benchContextStep(server *Server, configured config.Limits, normalized filters.Result) error {
	prompt, err := server.checkContext(configured, normalized)
	if err == nil {
		server.countPrompt(prompt)
	}
	return err
}
