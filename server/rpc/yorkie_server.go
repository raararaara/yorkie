/*
 * Copyright 2020 The Yorkie Authors. All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package rpc

import (
	"context"
	"fmt"
	gotime "time"

	"connectrpc.com/connect"

	"github.com/yorkie-team/yorkie/api/converter"
	"github.com/yorkie-team/yorkie/api/types"
	"github.com/yorkie-team/yorkie/api/types/events"
	api "github.com/yorkie-team/yorkie/api/yorkie/v1"
	"github.com/yorkie-team/yorkie/pkg/document"
	"github.com/yorkie-team/yorkie/pkg/document/key"
	"github.com/yorkie-team/yorkie/pkg/document/time"
	"github.com/yorkie-team/yorkie/server/backend"
	"github.com/yorkie-team/yorkie/server/backend/messagebroker"
	"github.com/yorkie-team/yorkie/server/backend/pubsub"
	"github.com/yorkie-team/yorkie/server/backend/sync"
	"github.com/yorkie-team/yorkie/server/clients"
	"github.com/yorkie-team/yorkie/server/documents"
	"github.com/yorkie-team/yorkie/server/logging"
	"github.com/yorkie-team/yorkie/server/packs"
	"github.com/yorkie-team/yorkie/server/projects"
	"github.com/yorkie-team/yorkie/server/rpc/auth"
)

type yorkieServer struct {
	backend    *backend.Backend
	serviceCtx context.Context
}

// newYorkieServer creates a new instance of yorkieServer
func newYorkieServer(serviceCtx context.Context, be *backend.Backend) *yorkieServer {
	return &yorkieServer{
		backend:    be,
		serviceCtx: serviceCtx,
	}
}

// ActivateClient activates the given client.
func (s *yorkieServer) ActivateClient(
	ctx context.Context,
	req *connect.Request[api.ActivateClientRequest],
) (*connect.Response[api.ActivateClientResponse], error) {
	if req.Msg.ClientKey == "" {
		return nil, clients.ErrInvalidClientKey
	}

	if err := auth.VerifyAccess(ctx, s.backend, &types.AccessInfo{
		Method: types.ActivateClient,
	}); err != nil {
		return nil, err
	}

	project := projects.From(ctx)
	cli, err := clients.Activate(ctx, s.backend, project, req.Msg.ClientKey, req.Msg.Metadata)
	if err != nil {
		return nil, err
	}

	if userID, exist := req.Msg.Metadata["userID"]; exist && userID != "" {
		if err := s.backend.MsgBroker.Produce(
			ctx,
			messagebroker.UserEventMessage{
				UserID:    userID,
				Timestamp: gotime.Now(),
				EventType: events.ClientActivatedEvent,
				ProjectID: project.ID.String(),
				UserAgent: req.Header().Get("x-yorkie-user-agent"),
			},
		); err != nil {
			logging.From(ctx).Error(err)
		}
	}

	return connect.NewResponse(&api.ActivateClientResponse{
		ClientId: cli.ID.String(),
	}), nil
}

// DeactivateClient deactivates the given client.
func (s *yorkieServer) DeactivateClient(
	ctx context.Context,
	req *connect.Request[api.DeactivateClientRequest],
) (*connect.Response[api.DeactivateClientResponse], error) {
	actorID, err := time.ActorIDFromHex(req.Msg.ClientId)
	if err != nil {
		return nil, err
	}

	if err := auth.VerifyAccess(ctx, s.backend, &types.AccessInfo{
		Method: types.DeactivateClient,
	}); err != nil {
		return nil, err
	}

	project := projects.From(ctx)
	_, err = clients.Deactivate(ctx, s.backend, project, types.ClientRefKey{
		ProjectID: project.ID,
		ClientID:  types.IDFromActorID(actorID),
	})
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&api.DeactivateClientResponse{}), nil
}

// AttachDocument attaches the given document to the client.
func (s *yorkieServer) AttachDocument(
	ctx context.Context,
	req *connect.Request[api.AttachDocumentRequest],
) (*connect.Response[api.AttachDocumentResponse], error) {
	actorID, err := time.ActorIDFromHex(req.Msg.ClientId)
	if err != nil {
		return nil, err
	}

	pack, err := converter.FromChangePack(req.Msg.ChangePack)
	if err != nil {
		return nil, err
	}
	if err := pack.DocumentKey.Validate(); err != nil {
		return nil, err
	}

	if err := auth.VerifyAccess(ctx, s.backend, &types.AccessInfo{
		Method:     types.AttachDocument,
		Attributes: auth.AccessAttributes(pack),
	}); err != nil {
		return nil, err
	}

	project := projects.From(ctx)
	locker, err := s.backend.Lockers.Locker(ctx, packs.DocEditKey(project.ID, pack.DocumentKey))
	if err != nil {
		return nil, err
	}
	if err := locker.Lock(ctx); err != nil {
		return nil, err
	}
	defer func() {
		if err := locker.Unlock(ctx); err != nil {
			logging.DefaultLogger().Error(err)
		}
	}()

	clientInfo, err := clients.FindActiveClientInfo(ctx, s.backend, types.ClientRefKey{
		ProjectID: project.ID,
		ClientID:  types.IDFromActorID(actorID),
	})
	if err != nil {
		return nil, err
	}
	docInfo, err := documents.FindDocInfoByKeyAndOwner(ctx, s.backend, clientInfo, pack.DocumentKey, true)
	if err != nil {
		return nil, err
	}

	if project.HasAttachmentLimit() {
		count, err := documents.FindAttachedClientCount(ctx, s.backend, types.DocRefKey{
			ProjectID: project.ID,
			DocID:     docInfo.ID,
		})
		if err != nil {
			return nil, err
		}

		if err := project.IsAttachmentLimitExceeded(count); err != nil {
			return nil, err
		}
	}

	if err := clientInfo.AttachDocument(docInfo.ID, pack.IsAttached()); err != nil {
		return nil, err
	}

	pulled, err := packs.PushPull(ctx, s.backend, project, clientInfo, docInfo, pack, packs.PushPullOptions{
		Mode:   types.SyncModePushPull,
		Status: document.StatusAttached,
	})
	if err != nil {
		return nil, err
	}

	pbChangePack, err := pulled.ToPBChangePack()
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&api.AttachDocumentResponse{
		ChangePack: pbChangePack,
		DocumentId: docInfo.ID.String(),
	}), nil
}

// DetachDocument detaches the given document to the client.
func (s *yorkieServer) DetachDocument(
	ctx context.Context,
	req *connect.Request[api.DetachDocumentRequest],
) (*connect.Response[api.DetachDocumentResponse], error) {
	actorID, err := time.ActorIDFromHex(req.Msg.ClientId)
	if err != nil {
		return nil, err
	}

	pack, err := converter.FromChangePack(req.Msg.ChangePack)
	if err != nil {
		return nil, err
	}
	docID, err := converter.FromDocumentID(req.Msg.DocumentId)
	if err != nil {
		return nil, err
	}

	if err := auth.VerifyAccess(ctx, s.backend, &types.AccessInfo{
		Method:     types.DetachDocument,
		Attributes: auth.AccessAttributes(pack),
	}); err != nil {
		return nil, err
	}

	project := projects.From(ctx)
	locker, err := s.backend.Lockers.Locker(ctx, packs.DocEditKey(project.ID, pack.DocumentKey))
	if err != nil {
		return nil, err
	}

	if err := locker.Lock(ctx); err != nil {
		return nil, err
	}
	defer func() {
		if err := locker.Unlock(ctx); err != nil {
			logging.DefaultLogger().Error(err)
		}
	}()

	clientInfo, err := clients.FindActiveClientInfo(ctx, s.backend, types.ClientRefKey{
		ProjectID: project.ID,
		ClientID:  types.IDFromActorID(actorID),
	})
	if err != nil {
		return nil, err
	}

	docRefKey := types.DocRefKey{
		ProjectID: project.ID,
		DocID:     docID,
	}
	docInfo, err := documents.FindDocInfoByRefKey(ctx, s.backend, docRefKey)
	if err != nil {
		return nil, err
	}

	isAttached, err := documents.IsDocumentAttached(
		ctx, s.backend,
		docRefKey,
		clientInfo.ID,
	)
	if err != nil {
		return nil, err
	}

	var status document.StatusType
	if req.Msg.RemoveIfNotAttached && !isAttached {
		pack.IsRemoved = true
		status = document.StatusRemoved
	} else {
		status = document.StatusDetached
	}

	pulled, err := packs.PushPull(ctx, s.backend, project, clientInfo, docInfo, pack, packs.PushPullOptions{
		Mode:   types.SyncModePushPull,
		Status: status,
	})
	if err != nil {
		return nil, err
	}

	pbChangePack, err := pulled.ToPBChangePack()
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&api.DetachDocumentResponse{
		ChangePack: pbChangePack,
	}), nil
}

// PushPullChanges stores the changes sent by the client and delivers the changes
// accumulated in the server to the client.
func (s *yorkieServer) PushPullChanges(
	ctx context.Context,
	req *connect.Request[api.PushPullChangesRequest],
) (*connect.Response[api.PushPullChangesResponse], error) {
	actorID, err := time.ActorIDFromHex(req.Msg.ClientId)
	if err != nil {
		return nil, err
	}

	pack, err := converter.FromChangePack(req.Msg.ChangePack)
	if err != nil {
		return nil, err
	}
	docID, err := converter.FromDocumentID(req.Msg.DocumentId)
	if err != nil {
		return nil, err
	}

	if err := auth.VerifyAccess(ctx, s.backend, &types.AccessInfo{
		Method:     types.PushPull,
		Attributes: auth.AccessAttributes(pack),
	}); err != nil {
		return nil, err
	}

	project := projects.From(ctx)

	if pack.HasChanges() {
		locker, err := s.backend.Lockers.Locker(
			ctx,
			packs.DocEditKey(project.ID, pack.DocumentKey),
		)
		if err != nil {
			return nil, err
		}

		if err := locker.Lock(ctx); err != nil {
			return nil, err
		}
		defer func() {
			if err := locker.Unlock(ctx); err != nil {
				logging.DefaultLogger().Error(err)
			}
		}()
	}

	clientInfo, err := clients.FindActiveClientInfo(ctx, s.backend, types.ClientRefKey{
		ProjectID: project.ID,
		ClientID:  types.IDFromActorID(actorID),
	})
	if err != nil {
		return nil, err
	}

	docRefKey := types.DocRefKey{
		ProjectID: project.ID,
		DocID:     docID,
	}
	docInfo, err := documents.FindDocInfoByRefKey(ctx, s.backend, docRefKey)
	if err != nil {
		return nil, err
	}

	if err := clientInfo.EnsureDocumentAttached(docInfo.ID); err != nil {
		return nil, err
	}

	syncMode := types.SyncModePushPull
	if req.Msg.PushOnly {
		syncMode = types.SyncModePushOnly
	}

	pulled, err := packs.PushPull(ctx, s.backend, project, clientInfo, docInfo, pack, packs.PushPullOptions{
		Mode:   syncMode,
		Status: document.StatusAttached,
	})
	if err != nil {
		return nil, err
	}

	pbChangePack, err := pulled.ToPBChangePack()
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&api.PushPullChangesResponse{
		ChangePack: pbChangePack,
	}), nil
}

// WatchDocument connects the stream to deliver events from the given documents
// to the requesting client.
func (s *yorkieServer) WatchDocument(
	ctx context.Context,
	req *connect.Request[api.WatchDocumentRequest],
	stream *connect.ServerStream[api.WatchDocumentResponse],
) error {
	clientID, err := time.ActorIDFromHex(req.Msg.ClientId)
	if err != nil {
		return err
	}

	project := projects.From(ctx)
	docID, err := converter.FromDocumentID(req.Msg.DocumentId)
	if err != nil {
		return err
	}
	docRefKey := types.DocRefKey{
		ProjectID: project.ID,
		DocID:     docID,
	}

	if _, err = clients.FindActiveClientInfo(ctx, s.backend, types.ClientRefKey{
		ProjectID: project.ID,
		ClientID:  types.IDFromActorID(clientID),
	}); err != nil {
		return err
	}

	docInfo, err := documents.FindDocInfoByRefKey(
		ctx,
		s.backend,
		docRefKey,
	)
	if err != nil {
		return nil
	}

	if err := auth.VerifyAccess(ctx, s.backend, &types.AccessInfo{
		Method:     types.WatchDocuments,
		Attributes: types.NewAccessAttributes([]key.Key{docInfo.Key}, types.Read),
	}); err != nil {
		return err
	}

	locker, err := s.backend.Lockers.Locker(
		ctx,
		sync.NewKey(fmt.Sprintf("watchdoc-%s-%s", clientID, docID)),
	)
	if err != nil {
		return err
	}
	if err := locker.Lock(ctx); err != nil {
		return err
	}
	defer func() {
		if err := locker.Unlock(context.Background()); err != nil {
			logging.DefaultLogger().Error(err)
		}
	}()

	subscription, clientIDs, err := s.watchDoc(
		ctx,
		clientID,
		docRefKey,
		project.MaxSubscribersPerDocument,
	)
	if err != nil {
		return err
	}

	s.backend.Metrics.AddWatchDocumentConnections(s.backend.Config.Hostname, project)
	defer func() {
		if err := s.unwatchDoc(ctx, subscription, docRefKey); err != nil {
			logging.From(ctx).Error(err)
		} else {
			s.backend.Metrics.RemoveWatchDocumentConnections(s.backend.Config.Hostname, project)
		}
	}()

	var pbClientIDs []string
	for _, id := range clientIDs {
		pbClientIDs = append(pbClientIDs, id.String())
	}
	if err := stream.Send(&api.WatchDocumentResponse{
		Body: &api.WatchDocumentResponse_Initialization_{
			Initialization: &api.WatchDocumentResponse_Initialization{
				ClientIds: pbClientIDs,
			},
		},
	}); err != nil {
		return err
	}

	for {
		select {
		case <-s.serviceCtx.Done():
			return context.Canceled
		case <-ctx.Done():
			return context.Canceled
		case event := <-subscription.Events():
			eventType, err := converter.ToDocEventType(event.Type)
			if err != nil {
				return err
			}

			response := &api.WatchDocumentResponse{
				Body: &api.WatchDocumentResponse_Event{
					Event: &api.DocEvent{
						Type:      eventType,
						Publisher: event.Publisher.String(),
						Body: &api.DocEventBody{
							Topic:   event.Body.Topic,
							Payload: event.Body.Payload,
						},
					},
				},
			}
			if err := stream.Send(response); err != nil {
				return err
			}
			s.backend.Metrics.AddWatchDocumentEventPayloadBytes(
				s.backend.Config.Hostname,
				project,
				event.Type,
				event.Body.PayloadLen(),
			)
		}
	}
}

// RemoveDocument removes the given document.
func (s *yorkieServer) RemoveDocument(
	ctx context.Context,
	req *connect.Request[api.RemoveDocumentRequest],
) (*connect.Response[api.RemoveDocumentResponse], error) {
	actorID, err := time.ActorIDFromHex(req.Msg.ClientId)
	if err != nil {
		return nil, err
	}

	pack, err := converter.FromChangePack(req.Msg.ChangePack)
	if err != nil {
		return nil, err
	}
	docID, err := converter.FromDocumentID(req.Msg.DocumentId)
	if err != nil {
		return nil, err
	}

	if err := auth.VerifyAccess(ctx, s.backend, &types.AccessInfo{
		Method:     types.RemoveDocument,
		Attributes: auth.AccessAttributes(pack),
	}); err != nil {
		return nil, err
	}

	project := projects.From(ctx)

	if pack.HasChanges() {
		locker, err := s.backend.Lockers.Locker(ctx, packs.DocEditKey(project.ID, pack.DocumentKey))
		if err != nil {
			return nil, err
		}

		if err := locker.Lock(ctx); err != nil {
			return nil, err
		}
		defer func() {
			if err := locker.Unlock(ctx); err != nil {
				logging.DefaultLogger().Error(err)
			}
		}()
	}

	clientInfo, err := clients.FindActiveClientInfo(ctx, s.backend, types.ClientRefKey{
		ProjectID: project.ID,
		ClientID:  types.IDFromActorID(actorID),
	})
	if err != nil {
		return nil, err
	}

	docRefKey := types.DocRefKey{
		ProjectID: project.ID,
		DocID:     docID,
	}
	docInfo, err := documents.FindDocInfoByRefKey(ctx, s.backend, docRefKey)
	if err != nil {
		return nil, err
	}

	pulled, err := packs.PushPull(ctx, s.backend, project, clientInfo, docInfo, pack, packs.PushPullOptions{
		Mode:   types.SyncModePushPull,
		Status: document.StatusRemoved,
	})
	if err != nil {
		return nil, err
	}

	pbChangePack, err := pulled.ToPBChangePack()
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&api.RemoveDocumentResponse{
		ChangePack: pbChangePack,
	}), nil
}

func (s *yorkieServer) watchDoc(
	ctx context.Context,
	clientID time.ActorID,
	docKey types.DocRefKey,
	limit int,
) (*pubsub.Subscription, []time.ActorID, error) {
	subscription, clientIDs, err := s.backend.PubSub.Subscribe(ctx, clientID, docKey, limit)
	if err != nil {
		return nil, nil, err
	}

	s.backend.PubSub.Publish(
		ctx,
		subscription.Subscriber(),
		events.DocEvent{
			Type:      events.DocWatchedEvent,
			Publisher: subscription.Subscriber(),
			DocRefKey: docKey,
		},
	)
	s.backend.Metrics.AddWatchDocumentEventPayloadBytes(
		s.backend.Config.Hostname,
		projects.From(ctx),
		events.DocWatchedEvent,
		0,
	)

	return subscription, clientIDs, nil
}

func (s *yorkieServer) unwatchDoc(
	ctx context.Context,
	subscription *pubsub.Subscription,
	docKey types.DocRefKey,
) error {
	s.backend.PubSub.Unsubscribe(ctx, docKey, subscription)
	s.backend.PubSub.Publish(
		ctx,
		subscription.Subscriber(),
		events.DocEvent{
			Type:      events.DocUnwatchedEvent,
			Publisher: subscription.Subscriber(),
			DocRefKey: docKey,
		},
	)
	s.backend.Metrics.AddWatchDocumentEventPayloadBytes(
		s.backend.Config.Hostname,
		projects.From(ctx),
		events.DocUnwatchedEvent,
		0,
	)

	return nil
}

func (s *yorkieServer) Broadcast(
	ctx context.Context,
	req *connect.Request[api.BroadcastRequest],
) (*connect.Response[api.BroadcastResponse], error) {
	clientID, err := time.ActorIDFromHex(req.Msg.ClientId)
	if err != nil {
		return nil, err
	}

	project := projects.From(ctx)
	docID, err := converter.FromDocumentID(req.Msg.DocumentId)
	if err != nil {
		return nil, err
	}
	docKey := types.DocRefKey{
		ProjectID: project.ID,
		DocID:     docID,
	}

	docInfo, err := documents.FindDocInfoByRefKey(
		ctx,
		s.backend,
		docKey,
	)
	if err != nil {
		return nil, err
	}

	// TODO(sejongk): It seems better to use a separate auth attributes for broadcast later
	if err := auth.VerifyAccess(ctx, s.backend, &types.AccessInfo{
		Method:     types.Broadcast,
		Attributes: types.NewAccessAttributes([]key.Key{docInfo.Key}, types.Read),
	}); err != nil {
		return nil, err
	}

	if _, err = clients.FindActiveClientInfo(ctx, s.backend, types.ClientRefKey{
		ProjectID: project.ID,
		ClientID:  types.IDFromActorID(clientID),
	}); err != nil {
		return nil, err
	}

	s.backend.PubSub.Publish(
		ctx,
		clientID,
		events.DocEvent{
			Type:      events.DocBroadcastEvent,
			Publisher: clientID,
			DocRefKey: docKey,
			Body: events.DocEventBody{
				Topic:   req.Msg.Topic,
				Payload: req.Msg.Payload,
			},
		},
	)
	s.backend.Metrics.AddWatchDocumentEventPayloadBytes(
		s.backend.Config.Hostname,
		project,
		events.DocBroadcastEvent,
		len(req.Msg.Payload),
	)

	return connect.NewResponse(&api.BroadcastResponse{}), nil
}
