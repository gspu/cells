/*
 * Copyright (c) 2018. Abstrium SAS <team (at) pydio.com>
 * This file is part of Pydio Cells.
 *
 * Pydio Cells is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * Pydio Cells is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with Pydio Cells.  If not, see <http://www.gnu.org/licenses/>.
 *
 * The latest code can be found at <https://pydio.com>.
 */

// Package consul embeds a Consul.io service for services discovery
package consul

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/hashicorp/consul/agent"
	"github.com/hashicorp/consul/agent/config"
	"github.com/pydio/cells/common"
	"github.com/pydio/cells/common/log"
	"github.com/pydio/cells/common/registry"
	"github.com/pydio/cells/common/service"
	"github.com/pydio/cells/common/service/context"
)

func init() {
	service.NewService(
		service.Name(common.SERVICE_CONSUL),
		service.Tag(common.SERVICE_TAG_DISCOVERY),
		service.Description("Service registry based on Consul.io"),
		service.BeforeInit(prerun),
		service.BeforeInit(func(_ service.Service) error {
			registry.Init(
				registry.Name(common.SERVICE_CONSUL),
			)
			return nil
		}),
		service.WithGeneric(func(ctx context.Context, cancel context.CancelFunc) (service.Runner, service.Checker, service.Stopper, error) {
			data, _ := json.Marshal(servicecontext.GetConfig(ctx))

			// Making sure bool are converted
			r := strings.NewReplacer(`"true"`, `true`, `"false"`, `false`)
			str := r.Replace(string(data))

			//create the logwriter
			runtime := config.DefaultRuntimeConfig(str)

			agent, err := agent.New(runtime)
			if err != nil {
				return nil, nil, nil, err
			}

			agent.LogOutput = &logwriter{ctx}

			return service.RunnerFunc(func() error {
					return agent.Start()
				}), service.CheckerFunc(func() error {
					return nil
				}), service.StopperFunc(func() error {
					agent.ShutdownAgent()

					return nil
				}), nil
		}),
	)
}

type logwriter struct {
	ctx context.Context
}

// Write to the lowest context for the standard logwriter
func (lw *logwriter) Write(p []byte) (n int, err error) {
	log.Logger(lw.ctx).Info(string(p))

	return len(p), nil
}
