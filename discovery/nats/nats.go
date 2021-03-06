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

package nats

import (
	broker "github.com/micro/go-plugins/broker/nats"
	registry "github.com/micro/go-plugins/registry/nats"
	transport "github.com/micro/go-plugins/transport/grpc"
	"github.com/pydio/cells/common/config"
	"github.com/pydio/cells/common/service"
	"github.com/pydio/cells/common/service/defaults"
)

func prerun(s service.Service) error {
	c := config.Get("cert", "grpc", "certFile").String("")
	k := config.Get("cert", "grpc", "keyFile").String("")

	defaults.Init(
		defaults.WithRegistry(registry.NewRegistry()),
		defaults.WithBroker(broker.NewBroker()),
		defaults.WithTransport(transport.NewTransport()),
		defaults.WithCert(c, k),
	)

	return nil
}
