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

package dex

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/coreos/dex/connector"
	"github.com/golang/protobuf/ptypes/any"
	"github.com/micro/go-micro/errors"
	"github.com/micro/protobuf/ptypes"
	"github.com/sirupsen/logrus"
	"go.uber.org/zap"

	"github.com/pydio/cells/common"
	"github.com/pydio/cells/common/log"
	"github.com/pydio/cells/common/proto/idm"
	"github.com/pydio/cells/common/service/defaults"
	"github.com/pydio/cells/common/service/proto"
	"github.com/pydio/cells/common/utils"
)

type WrapperConfig struct {
	Connectors []ConnectorConfig `json:"pydioconnectors"`
}

type ConnectorConfig struct {
	Type   string          `json:"type"`
	ID     int16           `json:"id"`
	Name   string          `json:"name"`
	IsLast bool            `json:islast`
	Config json.RawMessage `json:"config"`
}

func (c *WrapperConfig) Open(logger logrus.FieldLogger) (connector.Connector, error) {
	return c.OpenConnector(logger)
}

func (c *WrapperConfig) OpenConnector(logger logrus.FieldLogger) (interface {
	connector.Connector
	connector.PasswordConnector
	connector.RefreshConnector
}, error) {
	return c.openConnector(logger)
}

func (c *WrapperConfig) openConnector(logger logrus.FieldLogger) (*pydioWrapperConnector, error) {
	return &pydioWrapperConnector{*c, logger}, nil
}

type pydioWrapperConnector struct {
	WrapperConfig
	logger logrus.FieldLogger
}

var (
	_ connector.PasswordConnector = (*pydioWrapperConnector)(nil)
	_ connector.RefreshConnector  = (*pydioWrapperConnector)(nil)
)

/////////////////////////////////
// Connector Interface Methods
/////////////////////////////////

// Login binds a user by name / password, trying on various connectors.
func (p *pydioWrapperConnector) Login(ctx context.Context, s connector.Scopes, username, password string) (identity connector.Identity, validPassword bool, err error) {

	listConnector, err := p.getConnectorList(p.logger)

	for _, pydioConnector := range listConnector {

		if _, ok, err := pydioConnector.Connector.Login(ctx, s, username, password); !ok || err != nil {
			log.Logger(ctx).Debug("Login request failed on sub-connector", zap.String(common.KEY_USERNAME, username), zap.String(common.KEY_CONNECTOR, pydioConnector.Name), zap.Error(err))
			continue
		}

		log.Logger(ctx).Debug("Login request success on sub-connector", zap.String(common.KEY_USERNAME, username), zap.String(common.KEY_CONNECTOR, pydioConnector.Name))
		log.Auditer(ctx).Info(fmt.Sprintf("User %s logged in via %s sub-connector", username, pydioConnector.Name), log.GetAuditId(common.AUDIT_LOGIN_SUCCEED), zap.String(common.KEY_USERNAME, username), zap.String(common.KEY_CONNECTOR, pydioConnector.Name))

		return p.IdentityFromUserName(ctx, connector.Identity{Username: username, AuthSource: pydioConnector.Name})
	}

	log.Auditer(ctx).Error("Login attempt failed for "+username, log.GetAuditId(common.AUDIT_LOGIN_FAILED), zap.String(common.KEY_USERNAME, username))
	log.Logger(ctx).Error("login attempt failed", zap.String(common.KEY_USERNAME, username))
	return connector.Identity{}, false, nil
}

// Refresh reloads user info and checks pydio internal services for possible revokations.
// It does not list on connectors.
func (p *pydioWrapperConnector) Refresh(ctx context.Context, s connector.Scopes, ident connector.Identity) (connector.Identity, error) {

	newIdentiy, _, err := p.IdentityFromUserName(ctx, ident)
	return newIdentiy, err

}

///////////////////////
// Pydio API Methods
///////////////////////

// IdentityFromUserName reloads identity from pydio internal services.
func (p *pydioWrapperConnector) IdentityFromUserName(ctx context.Context, input connector.Identity) (output connector.Identity, authError bool, err error) {

	userClient := idm.NewUserServiceClient(common.SERVICE_GRPC_NAMESPACE_+common.SERVICE_USER, defaults.NewClient())
	singleQ, _ := ptypes.MarshalAny(&idm.UserSingleQuery{Login: input.Username})
	q := &service.Query{SubQueries: []*any.Any{singleQ}}
	streamer, err := userClient.SearchUser(ctx, &idm.SearchUserRequest{Query: q})
	if err != nil {
		log.Logger(ctx).Error("could not find user", zap.Error(err))
		return connector.Identity{}, false, err
	}
	defer streamer.Close()
	for {
		resp, e := streamer.Recv()
		if e != nil {
			break
		}
		if !p.CheckConnectionPolicyForUser(ctx, resp.User) {
			return connector.Identity{}, false, errors.Unauthorized(common.SERVICE_USER, "User "+input.Username+" is not authorized to log in")
		}
		return ConvertUserApiToIdentity(resp.User, input.AuthSource), true, nil
	}
	return connector.Identity{}, false, errors.NotFound(common.SERVICE_USER, "User "+input.Username+" not found")

}

// CheckConnectionPolicyForUser retrieves all subjects linked to current context and user.
// It then checks all relevant policies. If one has deny, it returns false.
func (p *pydioWrapperConnector) CheckConnectionPolicyForUser(ctx context.Context, user *idm.User) bool {

	cli := idm.NewPolicyEngineServiceClient(common.SERVICE_GRPC_NAMESPACE_+common.SERVICE_POLICY, defaults.NewClient())
	policyContext := make(map[string]string)
	utils.PolicyContextFromMetadata(policyContext, ctx)
	subjects := utils.PolicyRequestSubjectsFromUser(user)

	// Check all subjects, if one has deny return false
	policyRequest := &idm.PolicyEngineRequest{
		Subjects: subjects,
		Resource: "oidc",
		Action:   "login",
		Context:  policyContext,
	}
	if resp, err := cli.IsAllowed(ctx, policyRequest); err != nil || resp.Allowed == false {
		log.Auditer(ctx).Error(
			"policy denies login to "+user.Login,
			log.GetAuditId(common.AUDIT_LOGIN_POLICY_DENIAL),
			zap.String(common.KEY_USER_UUID, user.Uuid),
			zap.Any(common.KEY_POLICY_REQUEST, policyRequest),
			zap.Error(err),
		)
		log.Logger(ctx).Error("policy denies login for request", zap.Any(common.KEY_POLICY_REQUEST, policyRequest), zap.Error(err))
		return false
	}
	return true

}

func ConvertUserApiToIdentity(idmUser *idm.User, authSourceName string) (ident connector.Identity) {

	var roles []string
	for _, role := range idmUser.Roles {
		roles = append(roles, role.Uuid)
	}

	email, ok := idmUser.Attributes["email"]
	if !ok {
		email = ""
	}

	displayName, ok := idmUser.Attributes["displayName"]
	if !ok {
		displayName = ""
	}

	profile, ok := idmUser.Attributes["profile"]
	if !ok {
		profile = "standard"
	}

	return connector.Identity{
		UserID:        idmUser.Uuid,
		Username:      idmUser.Login,
		Email:         email,
		EmailVerified: true,
		Groups:        []string{},
		AuthSource:    authSourceName,
		DisplayName:   displayName,
		Roles:         roles,
		GroupPath:     idmUser.GetGroupPath(),
		Profile:       profile,
	}
}

///////////////////////
// Configs Methods
///////////////////////

// Lists connectors from config.
func (p *pydioWrapperConnector) getConnectorList(logger logrus.FieldLogger) (connectorList []ConnectorList, err error) {
	// Sort
	sort.Sort(byID(p.Connectors))
	// end sort
	for _, connConfig := range p.Connectors {
		connConnector, er := createConnector(logger, connConfig.Type, connConfig)
		if er != nil {
			logger.Errorf(er.Error())
		}
		connConnectorFull := ConnectorList{
			Type: connConfig.Type,
			Name: connConfig.Name,
			ID:   connConfig.ID,
			Connector: connConnector.(interface {
				connector.Connector
				connector.PasswordConnector
				connector.RefreshConnector
			}),
		}
		connectorList = append(connectorList, connConnectorFull)
	}
	return connectorList, nil
}

type ConnectorList struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	ID        int16  `json:"id"`
	IsLast    bool   `json:islast`
	Connector interface {
		connector.Connector
		connector.PasswordConnector
		connector.RefreshConnector
	}
}

// PydioConnector is a magical type that can unmarshal YAML dynamically. The
// Type field determines the connector type, which is then customized for Config.
type PydioConnector struct {
	Type   string               `json:"type"`
	Name   string               `json:"name"`
	ID     int16                `json:"id"`
	IsLast bool                 `json:islast`
	Config PydioConnectorConfig `json:"config"`
}

// createConnector parses the connector config and open the connector.
func createConnector(logger logrus.FieldLogger, connectorType string, connectorConfig ConnectorConfig) (connector.Connector, error) {
	var c connector.Connector

	if connectorConfig.Type == connectorType {
		//logger.Info("parse connector config: Type: Name == %s:%s", connectorConfig.Type, connectorConfig.Name)
		f, ok := PydioConnectorsConfig[connectorType]
		if !ok {
			return c, fmt.Errorf("unknown connector type %q", connectorType)
		}

		connConfig := f()
		if connectorConfig.Config != nil {
			//data := []byte(connectorConfig.Config)
			if err := json.Unmarshal(connectorConfig.Config, connConfig); err != nil {
				logger.Errorf("parse connector config: %v", err)
				return c, fmt.Errorf("parse connector config: %v", err)
			}
		}

		c, err := connConfig.Open(logger)
		if err != nil {
			logger.Errorf("failed to create connector %d - %s: %v", connectorConfig.ID, connectorConfig.Name, err)
			return c, fmt.Errorf("failed to create connector %d - %s: %v", connectorConfig.ID, connectorConfig.Name, err)
		}

		return c, nil
	}

	return nil, fmt.Errorf("unknown connector type %q", connectorType)
}

type byID []ConnectorConfig

func (n byID) Len() int           { return len(n) }
func (n byID) Less(i, j int) bool { return n[i].ID > n[j].ID }
func (n byID) Swap(i, j int)      { n[i], n[j] = n[j], n[i] }
