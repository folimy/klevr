package manager

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/context"
	"xorm.io/xorm"

	"github.com/Klevry/klevr/pkg/common"
	"github.com/NexClipper/logger"
	"github.com/gorilla/mux"
)

const (
	CHeaderApiKey         = "X-API-KEY"
	CHeaderAgentKey       = "X-AGENT-KEY"
	CHeaderHashCode       = "X-HASH-CODE"
	CHeaderZoneID         = "X-ZONE-ID"
	CHeaderSupportVersion = "X-SUPPORT-AGENT-VERSION"
	CHeaderTimestamp      = "X-TIMESTAMP"
)

// InitAgent initialize agent API
func (api *API) InitAgent(agent *mux.Router) {
	logger.Debug("API InitAgent - init URI")

	// registURI(agent, PUT, "/handshake", api.receiveHandshake)
	// registURI(agent, PUT, "/:agentKey", api.receivePolling)

	registURI(agent, PUT, "/handshake", api.receiveHandshake)
	registURI(agent, PUT, "/{agentKey}", api.receivePolling)
	registURI(agent, GET, "/reports/{agentKey}", api.checkPrimaryInfo)

	// agent API 핸들러 추가
	agent.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ch := parseCustomHeader(r)

			// TODO: Support agent version 입력 추가

			// APIKey 인증
			if !authenticate(w, r, ch.ZoneID, ch.APIKey) {
				return
			}

			// TODO: hash 검증 로직 추가

			next.ServeHTTP(w, r)

			// TODO: 전송구간 암호화 로직 추가

			// TODO: hash 생성 로직 추가

			// response header 설정
			h := w.Header()

			h.Set(CHeaderAgentKey, ch.AgentKey)
			h.Set(CHeaderHashCode, ch.HashCode)
			h.Set(CHeaderSupportVersion, ch.SupportVersion)
			h.Set(CHeaderTimestamp, string(time.Now().UTC().Unix()))
		})
	})
}

func authenticate(w http.ResponseWriter, r *http.Request, zoneID uint, apiKey string) bool {
	logger.Debug(r.RequestURI)

	if !existAPIKey(GetDBConn(r), apiKey, zoneID) {
		common.WriteHTTPError(401, w, nil, "authentication failed")
		return false
	}

	return true
}

func parseCustomHeader(r *http.Request) *common.CustomHeader {
	zoneID, _ := strconv.ParseUint(strings.Join(r.Header.Values(CHeaderZoneID), ""), 10, 64)
	ts, _ := strconv.ParseInt(strings.Join(r.Header.Values(CHeaderTimestamp), ""), 10, 64)

	h := &common.CustomHeader{
		APIKey:         strings.Join(r.Header.Values(CHeaderApiKey), ""),
		AgentKey:       strings.Join(r.Header.Values(CHeaderAgentKey), ""),
		HashCode:       strings.Join(r.Header.Values(CHeaderHashCode), ""),
		ZoneID:         uint(zoneID),
		SupportVersion: strings.Join(r.Header.Values(CHeaderSupportVersion), ""),
		Timestamp:      ts,
	}

	context.Set(r, common.CustomHeaderName, h)

	return h
}

func (api *API) receiveHandshake(w http.ResponseWriter, r *http.Request) {
	ch := common.GetCustomHeader(r)
	// var cr = &common.Request{r}
	var conn = GetDBConn(r)
	var rquestBody common.Body
	var paramAgent common.Me

	err := json.NewDecoder(r.Body).Decode(&rquestBody)
	if err != nil {
		common.WriteHTTPError(500, w, err, "JSON parsing error")
		return
	}

	paramAgent = rquestBody.Me
	logger.Debug(fmt.Sprintf("CustomHeader : %v", ch))

	group := getAgentGroup(conn, ch.ZoneID)

	if group.Id == 0 {
		common.WriteHTTPError(400, w, nil, fmt.Sprintf("Does not exist zone for zoneId : %d", ch.ZoneID))
		return
	}

	agent := getAgentByAgentKey(conn, ch.AgentKey)

	// agent 생성 or 수정
	upsertAgent(conn, agent, ch, &paramAgent)

	// response 데이터 생성
	rb := &common.Body{}

	// primary 조회
	rb.Agent.Primary = api.getPrimary(conn, ch.ZoneID, agent.Id)

	// 접속한 agent 정보
	me := &rb.Me

	me.HmacKey = agent.HmacKey
	me.EncKey = agent.EncKey
	// me.CallCycle = 0 // seconds
	// me.LogLevel = "DEBUG"

	b, err := json.Marshal(rb)
	if err != nil {
		panic(err)
	}

	w.WriteHeader(200)
	fmt.Fprintf(w, "%s", b)
}

func (api *API) receivePolling(w http.ResponseWriter, r *http.Request) {

}

func (api *API) checkPrimaryInfo(w http.ResponseWriter, r *http.Request) {
	ch := common.GetCustomHeader(r)
	// var cr = &common.Request{r}
	var conn = GetDBConn(r)
	var param common.Body

	err := json.NewDecoder(r.Body).Decode(&param)
	if err != nil {
		common.WriteHTTPError(500, w, err, "JSON parsing error")
		return
	}

	// response 데이터 생성
	rb := &common.Body{}

	agent := getAgentByAgentKey(conn, ch.AgentKey)

	// agent 접속 시간 갱신
	agent.LastAccessTime = time.Now().UTC()
	updateAgent(conn, agent)

	rb.Agent.Primary = api.getPrimary(conn, ch.ZoneID, agent.Id)

	b, err := json.Marshal(rb)
	if err != nil {
		panic(err)
	}

	w.WriteHeader(200)
	fmt.Fprintf(w, "%s", b)
}

func (api *API) getPrimary(conn *xorm.Session, zoneID uint, agentID uint) common.Primary {
	// primary agent 정보
	groupPrimary := getPrimaryAgent(conn, zoneID)
	var primaryAgent *Agents

	if groupPrimary.AgentId == 0 {
		primaryAgent = api.electPrimary(zoneID, agentID, false)
	} else {
		primaryAgent = getAgentByID(conn, groupPrimary.AgentId)

		if primaryAgent.Id == 0 {
			logger.Warning(fmt.Sprintf("primary agent not exist for id : %d", agentID))
			common.Throw(common.NewHTTPError(500, "primary agent not exist."))
		}

		// TODO: primary가 inactive인 경우 재선출 로직 추가
		old := primaryAgent.LastAccessTime.Add(1 * time.Second)
		now := time.Now().UTC()

		logger.Debugf("old origin : %v, old : %v, now : %v, before : %v", primaryAgent.LastAccessTime, old, now, old.Before(now))

		if old.Before(now) {
			primaryAgent = api.electPrimary(zoneID, agentID, true)
		}
	}

	return common.Primary{
		IP:             primaryAgent.Ip,
		Port:           primaryAgent.Port,
		IsActive:       primaryAgent.IsActive,
		LastAccessTime: primaryAgent.LastAccessTime.UTC().Unix(),
	}
}

// primary agent 선출
func (api *API) electPrimary(zoneID uint, agentID uint, oldDel bool) *Agents {
	logger.Debug("electPrimary for %d", zoneID)

	var conn *xorm.Session
	var agent *Agents

	common.Block{
		Try: func() {
			conn = api.DB.NewSession()

			if oldDel {
				deletePrimaryAgentIfOld(conn, zoneID, agentID, 30*time.Second)
			}

			pa := &PrimaryAgents{
				GroupId: zoneID,
				AgentId: agentID,
			}

			cnt, err := insertPrimaryAgent(conn, pa)

			if err != nil {
				pa = getPrimaryAgent(conn, zoneID)
			} else if cnt != 1 {
				logger.Warning(fmt.Sprintf("insert primary agent cnt : %d", cnt))
				common.Throw(common.NewStandardError("elect primary failed."))
			}

			if pa.AgentId == 0 {
				logger.Debugf("invalid primary agent : %v", pa)
				common.Throw(common.NewStandardError("elect primary failed."))
			}

			agent = getAgentByID(conn, pa.AgentId)

			if agent.Id == 0 {
				logger.Warning(fmt.Sprintf("primary agent not exist for id : %d, [%v]", agent.Id, agent))
				common.Throw(common.NewStandardError("elect primary failed."))
			}

			conn.Commit()
		},
		Catch: func(e common.Exception) {
			if conn != nil {
				conn.Rollback()
			}

			logger.Warning(e)
			common.Throw(e)
		},
		Finally: func() {
			if conn != nil && !conn.IsClosed() {
				conn.Close()
			}
		},
	}.Do()

	return agent
}

func hasLockAuth() bool {
	return true
}

func upsertAgent(conn *xorm.Session, agent *Agents, ch *common.CustomHeader, paramAgent *common.Me) {
	if agent.AgentKey == "" { // 처음 접속하는 에이전트일 경우 신규 등록
		agent.AgentKey = ch.AgentKey
		agent.GroupId = ch.ZoneID
		agent.IsActive = true
		agent.LastAccessTime = time.Now().UTC()
		agent.Ip = paramAgent.IP
		agent.Port = paramAgent.Port
		agent.Cpu = paramAgent.Resource.Core
		agent.Memory = paramAgent.Resource.Memory
		agent.Disk = paramAgent.Resource.Disk
		agent.HmacKey = common.GetKey(16)
		agent.EncKey = common.GetKey(32)

		addAgent(conn, agent)
	} else { // 기존에 등록된 에이전트 재접속일 경우 접속 정보 업데이트
		agent.IsActive = true
		agent.LastAccessTime = time.Now().UTC()
		agent.Ip = paramAgent.IP
		agent.Port = paramAgent.Port
		agent.Cpu = paramAgent.Resource.Core
		agent.Memory = paramAgent.Resource.Memory
		agent.Disk = paramAgent.Resource.Disk
		agent.HmacKey = common.GetKey(16)
		agent.EncKey = common.GetKey(32)

		updateAgent(conn, agent)
	}
}
