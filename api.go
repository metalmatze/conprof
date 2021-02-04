// Copyright 2018 The conprof Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"

	"google.golang.org/grpc"

	"github.com/conprof/conprof/pkg/store"
	"github.com/conprof/conprof/pkg/store/storepb"
)

type APICmd struct {
	httpFlags
	storeAddressFlag
	maxMergeBatchSizeFlag
}

func (cmd *APICmd) Run(cli *cliContext) error {
	conn, err := grpc.Dial(
		cmd.StoreAddress,
		grpc.WithInsecure(),
		grpc.WithUnaryInterceptor(
			otelgrpc.UnaryClientInterceptor(),
		),
		grpc.WithStreamInterceptor(
			otelgrpc.StreamClientInterceptor(),
		),
	)
	if err != nil {
		return err
	}
	c := storepb.NewReadableProfileStoreClient(conn)

	ws := NewWebserver(cmd.HTTPAddress,
		WebserverWithLogger(cli.logger),
		WebserverWithRegistry(cli.registry),
		WebserverGracePeriod(cmd.HTTPGracePeriod),
		WebserverAPIOnly(),
	)
	return ws.Run(store.NewGRPCQueryable(c), cmd.MaxMergeBatchSize, cli.reloadCh, cli.reloaders)
}
