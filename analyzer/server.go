/*
 * Copyright (C) 2015 Red Hat, Inc.
 *
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 *
 */

package analyzer

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/mux"

	"github.com/redhat-cip/skydive/api"
	"github.com/redhat-cip/skydive/config"
	"github.com/redhat-cip/skydive/flow"
	"github.com/redhat-cip/skydive/flow/mappings"
	"github.com/redhat-cip/skydive/logging"
	"github.com/redhat-cip/skydive/rpc"
	"github.com/redhat-cip/skydive/storage"
	"github.com/redhat-cip/skydive/storage/etcd"
	"github.com/redhat-cip/skydive/topology"
	"github.com/redhat-cip/skydive/topology/alert"
	"github.com/redhat-cip/skydive/topology/graph"
)

type Server struct {
	Addr                string
	Port                int
	running             atomic.Value
	Router              *mux.Router
	TopoServer          *topology.Server
	GraphServer         *graph.Server
	AlertServer         *alert.Server
	FlowMappingPipeline *mappings.FlowMappingPipeline
	Storage             storage.Storage
	FlowTable           *flow.FlowTable
	Conn                *net.UDPConn
	EmbeddedEtcd        *etcd.EmbeddedEtcd
}

func (s *Server) flowExpire(flows []*flow.Flow) {
	if s.Storage != nil {
		s.Storage.StoreFlows(flows)
		logging.GetLogger().Debugf("%d flows stored", len(flows))
	}
}

func (s *Server) AnalyzeFlows(flows []*flow.Flow) {
	s.FlowTable.Update(flows)
	s.FlowMappingPipeline.Enhance(flows)

	logging.GetLogger().Debugf("%d flows received", len(flows))
}

func (s *Server) handleUDPFlowPacket() {
	data := make([]byte, 4096)

	for s.running.Load() == true {
		n, _, err := s.Conn.ReadFromUDP(data)
		if err != nil {
			if s.running.Load() == false {
				return
			}
			logging.GetLogger().Errorf("Error while reading: %s", err.Error())
			return
		}

		f, err := flow.FromData(data[0:n])
		if err != nil {
			logging.GetLogger().Errorf("Error while parsing flow: %s", err.Error())
		}

		s.AnalyzeFlows([]*flow.Flow{f})
	}
}

func (s *Server) asyncFlowTableExpire() {
	for s.running.Load() == true {
		now := <-s.FlowTable.GetExpireTicker()
		s.FlowTable.Expire(now)
	}
}

func (s *Server) ListenAndServe() {
	var wg sync.WaitGroup
	s.running.Store(true)

	s.AlertServer.AlertManager.Start()

	wg.Add(5)
	go func() {
		defer wg.Done()
		s.TopoServer.ListenAndServe()
	}()

	go func() {
		defer wg.Done()
		s.GraphServer.ListenAndServe()
	}()

	go func() {
		defer wg.Done()
		s.AlertServer.ListenAndServe()
	}()

	go func() {
		defer wg.Done()

		addr, err := net.ResolveUDPAddr("udp", s.Addr+":"+strconv.FormatInt(int64(s.Port), 10))
		s.Conn, err = net.ListenUDP("udp", addr)
		if err != nil {
			panic(err)
		}
		defer s.Conn.Close()

		s.handleUDPFlowPacket()
	}()

	go func() {
		defer wg.Done()
		s.asyncFlowTableExpire()
	}()

	wg.Wait()
}

func (s *Server) Stop() {
	s.running.Store(false)
	s.FlowTable.UnregisterAll()
	s.AlertServer.Stop()
	s.TopoServer.Stop()
	s.GraphServer.Stop()
	if s.EmbeddedEtcd != nil {
		s.EmbeddedEtcd.Stop()
	}
	s.Conn.Close()
}

func (s *Server) Flush() {
	logging.GetLogger().Critical("Flush() MUST be called for testing purpose only, not in production")
	s.FlowTable.ExpireNow()
}

func (s *Server) FlowSearch(w http.ResponseWriter, r *http.Request) {
	filters := make(storage.Filters)
	for k, v := range r.URL.Query() {
		filters[k] = v[0]
	}

	flows, err := s.Storage.SearchFlows(filters)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(flows); err != nil {
		panic(err)
	}
}

func (s *Server) serveDataIndex(w http.ResponseWriter, r *http.Request, message string) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(message))
}

func (s *Server) ConversationLayer(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	layer := vars["layer"]

	ltype := flow.FlowEndpointType_ETHERNET
	switch layer {
	case "ethernet":
		ltype = flow.FlowEndpointType_ETHERNET
	case "ipv4":
		ltype = flow.FlowEndpointType_IPV4
	case "tcp":
		ltype = flow.FlowEndpointType_TCPPORT
	case "udp":
		ltype = flow.FlowEndpointType_UDPPORT
	case "sctp":
		ltype = flow.FlowEndpointType_SCTPPORT
	}
	s.serveDataIndex(w, r, s.FlowTable.JSONFlowConversationEthernetPath(ltype))
}

func (s *Server) DiscoveryType(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	discoType := vars["type"]
	dtype := flow.BYTES
	switch discoType {
	case "bytes":
		dtype = flow.BYTES
	case "packets":
		dtype = flow.PACKETS
	}
	s.serveDataIndex(w, r, s.FlowTable.JSONFlowDiscovery(dtype))
}

func (s *Server) RegisterRPCEndpoints() {
	routes := []rpc.Route{
		{
			"FlowSearch",
			"GET",
			"/rpc/flows",
			s.FlowSearch,
		},
		{
			"ConversationLayer",
			"GET",
			"/rpc/conversation/{layer}",
			s.ConversationLayer,
		},
		{
			"Discovery",
			"GET",
			"/rpc/discovery/{type}",
			s.DiscoveryType,
		},
	}

	rpc.RegisterRoutes(s.Router, routes)
}

func (s *Server) SetStorage(st storage.Storage) {
	s.Storage = st
}

func NewServer(addr string, port int, router *mux.Router, embedEtcd bool) (*Server, error) {
	backend, err := graph.BackendFromConfig()
	if err != nil {
		return nil, err
	}

	g, err := graph.NewGraph(backend)
	if err != nil {
		return nil, err
	}

	tserver := topology.NewServer("analyzer", g, addr, port, router)
	tserver.RegisterStaticEndpoints()
	tserver.RegisterRPCEndpoints()

	var etcdServer *etcd.EmbeddedEtcd
	if embedEtcd {
		if etcdServer, err = etcd.NewEmbeddedEtcdFromConfig(); err != nil {
			return nil, err
		}
	}

	etcdClient, err := etcd.NewEtcdClientFromConfig()
	if err != nil {
		return nil, err
	}

	apiServer, err := api.NewApi(router, etcdClient.KeysApi)
	if err != nil {
		return nil, err
	}

	alertManager := alert.NewAlertManager(g, apiServer.GetHandler("alert"))
	if err != nil {
		return nil, err
	}

	aserver, err := alert.NewServerFromConfig(alertManager, router)
	if err != nil {
		return nil, err
	}

	gserver, err := graph.NewServerFromConfig(g, router)
	if err != nil {
		return nil, err
	}

	gfe, err := mappings.NewGraphFlowEnhancer(g)
	if err != nil {
		return nil, err
	}

	pipeline := mappings.NewFlowMappingPipeline(gfe)

	flowtable := flow.NewFlowTable()

	server := &Server{
		Addr:                addr,
		Port:                port,
		Router:              router,
		TopoServer:          tserver,
		GraphServer:         gserver,
		AlertServer:         aserver,
		FlowMappingPipeline: pipeline,
		FlowTable:           flowtable,
		EmbeddedEtcd:        etcdServer,
	}
	server.RegisterRPCEndpoints()
	cfgFlowtable_expire := config.GetConfig().GetInt("analyzer.flowtable_expire")
	flowtable.RegisterExpire(server.flowExpire, time.Duration(cfgFlowtable_expire)*time.Minute)

	return server, nil
}

func NewServerFromConfig(router *mux.Router) (*Server, error) {
	addr, port, err := config.GetHostPortAttributes("analyzer", "listen")
	if err != nil {
		logging.GetLogger().Errorf("Configuration error: %s", err.Error())
		return nil, err
	}

	embedEtcd := config.GetConfig().GetBool("etcd.embedded")

	return NewServer(addr, port, router, embedEtcd)
}
