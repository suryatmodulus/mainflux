// Copyright (c) Mainflux
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	kithttp "github.com/go-kit/kit/transport/http"
	"github.com/go-zoo/bone"
	"github.com/mainflux/mainflux"
	"github.com/mainflux/mainflux/things"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const protocol = "http"

var (
	errMalformedData     = errors.New("malformed request data")
	errMalformedSubtopic = errors.New("malformed subtopic")
)

var (
	auth              mainflux.ThingsServiceClient
	channelPartRegExp = regexp.MustCompile(`^/channels/([\w\-]+)/messages(/[^?]*)?(\?.*)?$`)
)

// MakeHandler returns a HTTP handler for API endpoints.
func MakeHandler(svc mainflux.MessagePublisher, tc mainflux.ThingsServiceClient) http.Handler {
	opts := []kithttp.ServerOption{
		kithttp.ServerErrorEncoder(encodeError),
	}
	auth = tc

	r := bone.New()

	r.Post("/auth_on_register", kithttp.NewServer(
		authRegisterEndpoint(svc),
		decodeAuthRegister,
		encodeResponse,
		opts...,
	))

	r.Post("/auth_on_publish", kithttp.NewServer(
		authPublishEndpoint(svc),
		decodeAuthPublish,
		encodeResponse,
		opts...,
	))

	r.Post("/auth_on_subscribe", kithttp.NewServer(
		authSubscribeEndpoint(svc),
		decodeAuthSubscribe,
		encodeResponse,
		opts...,
	))

	r.GetFunc("/version", mainflux.Version("http"))
	r.Handle("/metrics", promhttp.Handler())

	return r
}

func decodeAuthRegister(_ context.Context, r *http.Request) (interface{}, error) {
	if !strings.Contains(r.Header.Get("vernemq-hook"), "auth_on_register") {
		return nil, errUnsupportedContentType
	}

	req := authRegisterReq{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}

	publisher, err := authenticate(req.password)
	if err != nil {
		return nil, err
	}

	return req, nil
}

func parseSubtopic(subtopic string) (string, error) {
	if subtopic == "" {
		return subtopic, nil
	}

	var err error
	subtopic, err = url.QueryUnescape(subtopic)
	if err != nil {
		return "", errMalformedSubtopic
	}
	subtopic = strings.Replace(subtopic, "/", ".", -1)

	elems := strings.Split(subtopic, ".")
	filteredElems := []string{}
	for _, elem := range elems {
		if elem == "" {
			continue
		}

		if len(elem) > 1 && (strings.Contains(elem, "*") || strings.Contains(elem, ">")) {
			return "", errMalformedSubtopic
		}

		filteredElems = append(filteredElems, elem)
	}

	subtopic = strings.Join(filteredElems, ".")
	return subtopic, nil
}

func decodeAuthPublish(_ context.Context, r *http.Request) (interface{}, error) {
	if !strings.Contains(r.Header.Get("vernemq-hook"), "auth_on_publish") {
		return nil, errUnsupportedContentType
	}

	req := authPublishReq{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}

	channelParts := channelPartRegExp.FindStringSubmatch(req.topic)
	if len(channelParts) < 2 {
		return nil, errMalformedData
	}
	chanID := channelParts[1]
	subtopic, err := parseSubtopic(channelParts[2])
	if err != nil {
		return nil, err
	}

	publisher, err := authorize(req.username, chanID)
	if err != nil {
		return nil, err
	}

	msg := mainflux.RawMessage{
		Publisher:   publisher,
		Protocol:    "mqtt",
		ContentType: r.Header.Get("Content-Type"),
		Channel:     chanID,
		Subtopic:    subtopic,
		Payload:     req.payload,
	}

	return msg, nil
}

func decodeAuthSubscribe(_ context.Context, r *http.Request) (interface{}, error) {
	if !strings.Contains(r.Header.Get("vernemq-hook"), "auth_on_subscribe") {
		return nil, errUnsupportedContentType
	}

	req := authSubscribeReq{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}

	channelParts := channelPartRegExp.FindStringSubmatch(req.topic)
	if len(channelParts) < 2 {
		return nil, errMalformedData
	}
	chanID := channelParts[1]
	_, err := authorize(req.username, chanID)
	if err != nil {
		return nil, err
	}

	return req, nil
}

func authenticate(apiKey string) (string, error) {
	if apiKey == "" {
		return "", things.ErrUnauthorizedAccess
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	id, err := auth.Identify(ctx, &mainflux.Token{Value: apiKey})
	if err != nil {
		return "", err
	}

	return id.GetValue(), nil
}

func authorize(apiKey, chanID string) (string, error) {
	if apiKey == "" {
		return "", things.ErrUnauthorizedAccess
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	id, err := auth.CanAccess(ctx, &mainflux.AccessReq{Token: apiKey, ChanID: chanID})
	if err != nil {
		return "", err
	}

	return id.GetValue(), nil
}

func encodeResponse(_ context.Context, w http.ResponseWriter, response interface{}) error {
	w.WriteHeader(http.StatusAccepted)
	return nil
}

func encodeError(_ context.Context, err error, w http.ResponseWriter) {
	switch err {
	case errMalformedData, errMalformedSubtopic:
		w.WriteHeader(http.StatusBadRequest)
	case things.ErrUnauthorizedAccess:
		w.WriteHeader(http.StatusForbidden)
	default:
		if e, ok := status.FromError(err); ok {
			switch e.Code() {
			case codes.PermissionDenied:
				w.WriteHeader(http.StatusForbidden)
			default:
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}
}