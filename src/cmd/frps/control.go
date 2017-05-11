// Copyright 2016 fatedier, fatedier@gmail.com
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/robscc/frp/src/models/consts"
	"github.com/robscc/frp/src/models/metric"
	"github.com/robscc/frp/src/models/msg"
	"github.com/robscc/frp/src/models/server"
	"github.com/robscc/frp/src/utils/conn"
	"github.com/robscc/frp/src/utils/log"
	"github.com/robscc/frp/src/utils/pcrypto"
)

func ProcessControlConn(l *conn.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		log.Debug("Get new connection, %v", c.GetRemoteAddr())
		go controlWorker(c)
	}
}

// connection from every client and server
func controlWorker(c *conn.Conn) {
	// if login message type is NewWorkConn, don't close this connection
	var closeFlag bool = true
	var s *server.ProxyServer
	defer func() {
		if closeFlag {
			c.Close()
			if s != nil {
				s.Close()
			}
		}
	}()

	// get login message
	buf, err := c.ReadLine()
	if err != nil {
		log.Warn("Read error, %v", err)
		return
	}
	log.Debug("Get msg from frpc: %s", buf)

	cliReq := &msg.ControlReq{}
	if err := json.Unmarshal([]byte(buf), &cliReq); err != nil {
		log.Warn("Parse msg from frpc error: %v : %s", err, buf)
		return
	}

	// login when type is NewCtlConn or NewWorkConn
	ret, info, s := doLogin(cliReq, c)
	// if login type is NewWorkConn, nothing will be send to frpc
	if cliReq.Type == consts.NewCtlConn {
		cliRes := &msg.ControlRes{
			Type: consts.NewCtlConnRes,
			Code: ret,
			Msg:  info,
		}
		byteBuf, _ := json.Marshal(cliRes)
		err = c.WriteString(string(byteBuf) + "\n")
		if err != nil {
			log.Warn("ProxyName [%s], write to client error, proxy exit", cliReq.ProxyName)
			return
		}
	} else {
		if ret == 0 {
			closeFlag = false
		}
		return
	}

	// if login failed, just return
	if ret > 0 {
		return
	}

	// create a channel for sending messages
	msgSendChan := make(chan interface{}, 1024)
	go msgSender(s, c, msgSendChan)
	go noticeUserConn(s, msgSendChan)

	// loop for reading control messages from frpc and deal with different types
	msgReader(s, c, msgSendChan)

	close(msgSendChan)
	log.Info("ProxyName [%s], I'm dead!", s.Name)
	return
}

// when frps get one new user connection, send NoticeUserConn message to frpc and accept one new WorkConn later
func noticeUserConn(s *server.ProxyServer, msgSendChan chan interface{}) {
	for {
		closeFlag := s.WaitUserConn()
		if closeFlag {
			log.Debug("ProxyName [%s], goroutine for noticing user conn is closed", s.Name)
			break
		}
		notice := &msg.ControlRes{
			Type: consts.NoticeUserConn,
		}
		msgSendChan <- notice
		log.Debug("ProxyName [%s], notice client to add work conn", s.Name)
	}
}

// loop for reading messages from frpc after control connection is established
func msgReader(s *server.ProxyServer, c *conn.Conn, msgSendChan chan interface{}) error {
	// for heartbeat
	var heartbeatTimeout bool = false
	timer := time.AfterFunc(time.Duration(server.HeartBeatTimeout)*time.Second, func() {
		heartbeatTimeout = true
		s.Close()
		log.Error("ProxyName [%s], client heartbeat timeout", s.Name)
	})
	defer timer.Stop()

	for {
		buf, err := c.ReadLine()
		if err != nil {
			if err == io.EOF {
				log.Warn("ProxyName [%s], client is dead!", s.Name)
				s.Close()
				return err
			} else if c == nil || c.IsClosed() {
				log.Warn("ProxyName [%s], client connection is closed", s.Name)
				s.Close()
				return err
			}
			log.Warn("ProxyName [%s], read error: %v", s.Name, err)
			continue
		}

		cliReq := &msg.ControlReq{}
		if err := json.Unmarshal([]byte(buf), &cliReq); err != nil {
			log.Warn("ProxyName [%s], parse msg from frpc error: %v : %s", s.Name, err, buf)
			continue
		}

		switch cliReq.Type {
		case consts.HeartbeatReq:
			log.Debug("ProxyName [%s], get heartbeat", s.Name)
			timer.Reset(time.Duration(server.HeartBeatTimeout) * time.Second)
			heartbeatRes := &msg.ControlRes{
				Type: consts.HeartbeatRes,
			}
			msgSendChan <- heartbeatRes
		default:
			log.Warn("ProxyName [%s}, unsupport msgType [%d]", s.Name, cliReq.Type)
		}
	}
	return nil
}

// loop for sending messages from channel to frpc
func msgSender(s *server.ProxyServer, c *conn.Conn, msgSendChan chan interface{}) {
	for {
		msg, ok := <-msgSendChan
		if !ok {
			break
		}

		buf, _ := json.Marshal(msg)
		err := c.WriteString(string(buf) + "\n")
		if err != nil {
			log.Warn("ProxyName [%s], write to client error, proxy exit", s.Name)
			s.Close()
			break
		}
	}
}

// if success, ret equals 0, otherwise greater than 0
// NewCtlConn
// NewWorkConn
// NewWorkConnUdp
func doLogin(req *msg.ControlReq, c *conn.Conn) (ret int64, info string, s *server.ProxyServer) {
	ret = 1
	// check if PrivilegeMode is enabled
	if req.PrivilegeMode && !server.PrivilegeMode {
		info = fmt.Sprintf("ProxyName [%s], PrivilegeMode is disabled in frps", req.ProxyName)
		log.Warn("info")
		return
	}

	var ok bool
	s, ok = server.GetProxyServer(req.ProxyName)
	if req.PrivilegeMode && req.Type == consts.NewCtlConn {
		log.Debug("ProxyName [%s], doLogin and privilege mode is enabled", req.ProxyName)
	} else {
		if !ok {
			info = fmt.Sprintf("ProxyName [%s] is not exist", req.ProxyName)
			log.Warn(info)
			return
		}
	}

	// check authKey or privilegeKey
	nowTime := time.Now().Unix()
	if req.PrivilegeMode {
		privilegeKey := pcrypto.GetAuthKey(req.ProxyName + server.PrivilegeToken + fmt.Sprintf("%d", req.Timestamp))
		// privilegeKey unavaiable after server.AuthTimeout minutes
		if server.AuthTimeout != 0 && nowTime-req.Timestamp > server.AuthTimeout {
			info = fmt.Sprintf("ProxyName [%s], privilege mode authorization timeout", req.ProxyName)
			log.Warn(info)
			return
		} else if req.PrivilegeKey != privilegeKey {
			info = fmt.Sprintf("ProxyName [%s], privilege mode authorization failed", req.ProxyName)
			log.Warn(info)
			log.Debug("PrivilegeKey [%s] and get [%s]", privilegeKey, req.PrivilegeKey)
			return
		}
	} else {
		authKey := pcrypto.GetAuthKey(req.ProxyName + s.AuthToken + fmt.Sprintf("%d", req.Timestamp))
		if server.AuthTimeout != 0 && nowTime-req.Timestamp > server.AuthTimeout {
			info = fmt.Sprintf("ProxyName [%s], authorization timeout", req.ProxyName)
			log.Warn(info)
			return
		} else if req.AuthKey != authKey {
			info = fmt.Sprintf("ProxyName [%s], authorization failed", req.ProxyName)
			log.Warn(info)
			log.Debug("AuthKey [%s] and get [%s]", authKey, req.AuthKey)
			return
		}
	}

	// control conn
	if req.Type == consts.NewCtlConn {
		if req.PrivilegeMode {
			s = server.NewProxyServerFromCtlMsg(req)
			// we check listen_port if privilege_allow_ports are set
			// and PrivilegeMode is enabled
			if s.Type == "tcp" {
				if len(server.PrivilegeAllowPorts) != 0 {
					_, ok := server.PrivilegeAllowPorts[s.ListenPort]
					if !ok {
						info = fmt.Sprintf("ProxyName [%s], remote_port [%d] isn't allowed", req.ProxyName, s.ListenPort)
						log.Warn(info)
						return
					}
				}
			} else if s.Type == "http" || s.Type == "https" {
				for _, domain := range s.CustomDomains {
					if server.SubDomainHost != "" && strings.Contains(domain, server.SubDomainHost) {
						info = fmt.Sprintf("ProxyName [%s], custom domain [%s] should not belong to subdomain_host [%s]", req.ProxyName, domain, server.SubDomainHost)
						log.Warn(info)
						return
					}
				}

				if s.SubDomain != "" {
					if strings.Contains(s.SubDomain, ".") || strings.Contains(s.SubDomain, "*") {
						info = fmt.Sprintf("ProxyName [%s], '.' and '*' is not supported in subdomain", req.ProxyName)
						log.Warn(info)
						return
					}
					if server.SubDomainHost == "" {
						info = fmt.Sprintf("ProxyName [%s], subdomain is not supported because this feature is not enabled by remote server", req.ProxyName)
						log.Warn(info)
						return
					}
					s.SubDomain = s.SubDomain + "." + server.SubDomainHost
				}
			}
			err := server.CreateProxy(s)
			if err != nil {
				info = fmt.Sprintf("ProxyName [%s], %v", req.ProxyName, err)
				log.Warn(info)
				return
			}
		}

		// check if vhost_port is set
		if s.Type == "http" && server.VhostHttpMuxer == nil {
			info = fmt.Sprintf("ProxyName [%s], type [http] not support when vhost_http_port is not set", req.ProxyName)
			log.Warn(info)
			return
		}
		if s.Type == "https" && server.VhostHttpsMuxer == nil {
			info = fmt.Sprintf("ProxyName [%s], type [https] not support when vhost_https_port is not set", req.ProxyName)
			log.Warn(info)
			return
		}

		// set infomations from frpc
		s.BindAddr = server.BindAddr
		s.UseEncryption = req.UseEncryption
		s.UseGzip = req.UseGzip
		s.HostHeaderRewrite = req.HostHeaderRewrite
		s.HttpUserName = req.HttpUserName
		s.HttpPassWord = req.HttpPassWord

		if req.PoolCount > server.MaxPoolCount {
			s.PoolCount = server.MaxPoolCount
		} else if req.PoolCount < 0 {
			s.PoolCount = 0
		} else {
			s.PoolCount = req.PoolCount
		}

		if s.Status == consts.Working {
			info = fmt.Sprintf("ProxyName [%s], already in use", req.ProxyName)
			log.Warn(info)
			return
		}

		// update metric's proxy status
		metric.SetProxyInfo(s.Name, s.Type, s.BindAddr, s.UseEncryption, s.UseGzip, s.PrivilegeMode, s.CustomDomains, s.Locations, s.ListenPort)

		// start proxy and listen for user connections, no block
		err := s.Start(c)
		if err != nil {
			info = fmt.Sprintf("ProxyName [%s], start proxy error: %v", req.ProxyName, err)
			log.Warn(info)
			return
		}
		log.Info("ProxyName [%s], start proxy success", req.ProxyName)
		if req.PrivilegeMode {
			log.Info("ProxyName [%s], created by PrivilegeMode", req.ProxyName)
		}
	} else if req.Type == consts.NewWorkConn {
		// work conn
		if s.Status != consts.Working {
			log.Warn("ProxyName [%s], is not working when it gets one new work connnection", req.ProxyName)
			return
		}
		// the connection will close after join over
		s.RegisterNewWorkConn(c)
	} else if req.Type == consts.NewWorkConnUdp {
		// work conn for udp
		if s.Status != consts.Working {
			log.Warn("ProxyName [%s], is not working when it gets one new work connnection for udp", req.ProxyName)
			return
		}
		s.RegisterNewWorkConnUdp(c)
	} else {
		info = fmt.Sprintf("Unsupport login message type [%d]", req.Type)
		log.Warn("Unsupport login message type [%d]", req.Type)
		return
	}

	ret = 0
	return
}
