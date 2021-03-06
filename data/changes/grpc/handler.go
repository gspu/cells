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

package grpc

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/pydio/cells/common"
	"github.com/pydio/cells/common/log"
	"github.com/pydio/cells/common/proto/jobs"
	"github.com/pydio/cells/common/proto/sync"
	"github.com/pydio/cells/common/proto/tree"
	"github.com/pydio/cells/common/service/context"
	"github.com/pydio/cells/common/service/defaults"
	"github.com/pydio/cells/data/changes"
)

// Handler to GRPC interface to changes API
type Handler struct{}

// OnTreeEvent receives events about node changes from the router
func (h Handler) OnTreeEvent(ctx context.Context, event *tree.NodeChangeEvent) error {
	if servicecontext.GetDAO(ctx) == nil {
		return fmt.Errorf("no DAO found, wrong initialization")
	}
	dao := servicecontext.GetDAO(ctx).(changes.DAO)

	change := &tree.SyncChange{}
	var refNode *tree.Node
	if event.GetSource() != nil {
		change.Source = event.GetSource().GetPath()
		change.NodeId = event.GetSource().Uuid
		refNode = event.GetSource()
	}
	if event.GetTarget() != nil {
		change.Target = event.GetTarget().GetPath()
		change.NodeId = event.GetTarget().Uuid
		refNode = event.GetTarget()
	}

	if change.NodeId == "" || refNode == nil {
		log.Logger(ctx).Error("cannot store change without NodeId", zap.Any(common.KEY_NODE_CHANGE_EVENT, change))
		return nil
	}

	if strings.HasSuffix(refNode.GetPath(), common.PYDIO_SYNC_HIDDEN_FILE_META) {
		// Ignore
		return nil
	}

	switch event.Type {
	case tree.NodeChangeEvent_CREATE:
		change.Type = tree.SyncChange_create
	case tree.NodeChangeEvent_DELETE:
		change.Type = tree.SyncChange_delete
	case tree.NodeChangeEvent_UPDATE_PATH:
		change.Type = tree.SyncChange_path
	case tree.NodeChangeEvent_UPDATE_CONTENT:
		change.Type = tree.SyncChange_content
		if change.Source == "" {
			change.Source = change.Target
		}
	default:
		return nil
	}

	log.Logger(ctx).Debug("NodeChangeEvent received", zap.Any(common.KEY_NODE_CHANGE_EVENT, change))
	err := dao.Put(change)
	if err != nil {
		log.Logger(ctx).Error("cannot store Change", zap.Any(common.KEY_NODE_CHANGE_EVENT, change), zap.Error(err))
	}
	return err
}

// TriggerResync allows a resync to be triggered by the pydio client
func (h Handler) TriggerResync(ctx context.Context, req *sync.ResyncRequest, resp *sync.ResyncResponse) error {

	log.Logger(ctx).Info("[Changes] Starting Resync Action For Changes")

	if servicecontext.GetDAO(ctx) == nil {
		return fmt.Errorf("no DAO found, wrong initialization")
	}
	dao := servicecontext.GetDAO(ctx).(changes.DAO)

	var taskClient jobs.JobServiceClient
	var theTask *jobs.Task
	if req.Task != nil {
		taskClient = jobs.NewJobServiceClient(common.SERVICE_GRPC_NAMESPACE_+common.SERVICE_JOBS, defaults.NewClient())
		theTask = req.Task
		theTask.StartTime = int32(time.Now().Unix())
	}

	indexClient := tree.NewNodeProviderClient(common.SERVICE_GRPC_NAMESPACE_+common.SERVICE_TREE, defaults.NewClient())
	streamer, err := indexClient.ListNodes(ctx, &tree.ListNodesRequest{
		Node: &tree.Node{
			Path: "",
		},
		Recursive: true,
	})
	if err != nil {
		if req.Task != nil {
			theTask.StatusMessage = err.Error()
			theTask.Status = jobs.TaskStatus_Error
			theTask.EndTime = int32(time.Now().Unix())
			theTask.ActionsLogs = append(theTask.ActionsLogs, &jobs.ActionLog{
				OutputMessage: &jobs.ActionMessage{OutputChain: []*jobs.ActionOutput{{ErrorString: err.Error()}}},
			})
			taskClient.PutTask(ctx, &jobs.PutTaskRequest{Task: theTask})
		}
		return err
	}
	defer streamer.Close()
	for {
		listResp, e := streamer.Recv()
		if e != nil {
			break
		}
		if listResp == nil {
			continue
		}
		node := listResp.Node
		if strings.HasSuffix(node.GetPath(), common.PYDIO_SYNC_HIDDEN_FILE_META) {
			continue
		}
		if ok, err := dao.HasNodeById(node.Uuid); ok && err == nil {
			continue
		}
		log.Logger(ctx).Debug("Manually creating node during changes re-indexation", node.Zap())
		dao.Put(&tree.SyncChange{
			Type:   tree.SyncChange_create,
			NodeId: node.Uuid,
			Source: "",
			Target: node.Path,
		})
		if req.Task != nil {
			theTask.StatusMessage = "Indexing node " + node.GetPath()
			theTask.Status = jobs.TaskStatus_Running
			theTask.ActionsLogs = append(theTask.ActionsLogs, &jobs.ActionLog{
				OutputMessage: &jobs.ActionMessage{OutputChain: []*jobs.ActionOutput{{StringBody: theTask.StatusMessage}}},
			})
			taskClient.PutTask(ctx, &jobs.PutTaskRequest{Task: theTask})
		}
	}

	if req.Task != nil {
		theTask.StatusMessage = "Complete"
		theTask.Status = jobs.TaskStatus_Finished
		theTask.EndTime = int32(time.Now().Unix())
		taskClient.PutTask(ctx, &jobs.PutTaskRequest{Task: theTask})
	}
	return nil

}

// Put a change in the service storage
func (h Handler) Put(ctx context.Context, stream tree.SyncChanges_PutStream) error {

	log.Logger(ctx).Debug("Put")

	if servicecontext.GetDAO(ctx) == nil {
		return fmt.Errorf("no DAO found, wrong initialization")
	}
	dao := servicecontext.GetDAO(ctx).(changes.DAO)

	for {
		change, err := stream.Recv()
		if err == io.EOF {
			return stream.Close()
		}

		if err != nil {
			return err
		}

		log.Logger(ctx).Debug("Put", zap.Any("change", change))

		if err := dao.Put(change); err != nil {
			log.Logger(ctx).Error("Put", zap.Error(err))
		}
	}
}

// Search a change in the service storage
func (h Handler) Search(ctx context.Context, req *tree.SearchSyncChangeRequest, stream tree.SyncChanges_SearchStream) error {

	log.Logger(ctx).Debug("Search")

	defer stream.Close()

	if servicecontext.GetDAO(ctx) == nil {
		return fmt.Errorf("no DAO found, wrong initialization")
	}
	dao := servicecontext.GetDAO(ctx).(changes.DAO)

	if req.GetLastSeqOnly() {

		lastSeq, err := dao.LastSeq()
		if err != nil {
			return err
		}
		return stream.Send(&tree.SyncChange{Seq: lastSeq})
	}

	res, err := dao.Get(req.GetSeq(), req.GetPrefix())
	if err != nil {
		log.Logger(ctx).Error("Search", zap.Error(err))
		return err
	}
	return changes.NewOptimizer(ctx, changes.ChangeChan(res)).Output(ctx, stream)
}
