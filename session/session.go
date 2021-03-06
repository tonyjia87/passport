package session

import (
	"container/list"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	log "github.com/golang/glog"
)

var (
	defaultSessionManager *SessionManager = nil

	stores = make(map[string]SessionStoreType)
)

type SessionStoreType func(interface{}) (SessionStore, error)
type PrepireReleaseFunc func(SessionStore) // 会话销毁时回调的函数

type SessionStore interface {
	Id(string) string                // 获取/设置会话ID
	CreateTime() int64               // 获取会话创建时间
	Active(set bool) int64           // 获取/设置最后活动时间
	Keys() []interface{}             // 获取所有二级键
	Get(key interface{}) interface{} // 获取一个键值
	Set(key, val interface{}) error  // 设置一个键值
	Delete(key interface{}) error    // 删除一个键值
	Release()                        // 销毁该会话
}

////
type SessionManager struct {
	Domain       string      `json:"domain"`
	StoreType    string      `json:"store_type"`
	CookieName   string      `json:"cookie_name"`
	IdleTime     int64       `json:"idle_time"`
	CookieExpire int         `json:"cookie_expire"`
	StoreConfig  interface{} `json:"store_config"`

	prepireRelease PrepireReleaseFunc       // 会话过期时的回调
	sessions       map[string]*list.Element // 本系统所有管理的会话
	list           *list.List
	lock           sync.RWMutex
	destroied      bool
}

func RegisterSessionStore(name string, one SessionStoreType) {
	if one == nil {
		panic("Register SessionStore nil")
	}

	if _, dup := stores[name]; dup {
		panic("Register SessionStore duplicate for " + name)
	}

	stores[name] = one
}

func newSessionStore(typeName string, config interface{}) (SessionStore, error) {
	if newFunc, ok := stores[typeName]; ok {
		return newFunc(config)
	}

	return nil, fmt.Errorf("No SessionManager types " + typeName)
}

func NewSessionManager(sessionConfig interface{}) (m *SessionManager) {
	if sessionConfig == nil {
		return nil
	}

	m = &SessionManager{}

	var byteConf []byte
	var err error
	if byteConf, err = json.Marshal(sessionConfig); err != nil {
		return nil
	}

	dec := json.NewDecoder(strings.NewReader(string(byteConf)))
	dec.UseNumber()
	if err := dec.Decode(m); err != nil {
		panic(err)

	}

	m.sessions = make(map[string]*list.Element)
	m.list = list.New()
	m.gc()

	return m
}

func (p *SessionManager) GetSession(w http.ResponseWriter, r *http.Request, sessionid string) (session SessionStore, err error) {
	writeCookie := false
	sid := ""

	cookie, errs := r.Cookie(p.CookieName)
	if errs != nil || cookie.Value == "" {
		if sessionid == "" {
			sid, err = p.sessionId()
		} else {
			sid = sessionid
		}
		writeCookie = true
	} else {
		sid, err = url.QueryUnescape(cookie.Value)
	}
	if err != nil {
		return
	}

	if sess, ok := p.sessions[sid]; ok {
		session = sess.Value.(SessionStore)
		session.Active(true)
		p.lock.Lock()
		p.list.MoveToBack(sess)
		p.lock.Unlock()
		return
	}

	// 新会话
	session, err = newSessionStore(p.StoreType, p.StoreConfig)
	if err != nil {
		return
	}
	session.Id(sid)

	p.lock.Lock()
	p.sessions[sid] = p.list.PushBack(session)
	p.lock.Unlock()

	if writeCookie == true {
		cookie = &http.Cookie{
			Name:   p.CookieName,
			Value:  url.QueryEscape(sid),
			Path:   "/",
			Domain: p.Domain,
		}

		if p.CookieExpire >= 0 {
			cookie.MaxAge = p.CookieExpire
		}

		http.SetCookie(w, cookie)
	}

	r.AddCookie(cookie)

	return
}

func (p *SessionManager) SessionDestroy(w http.ResponseWriter, r *http.Request) (sessionid string) {
	cookie, err := r.Cookie(p.CookieName)
	if err != nil || cookie.Value == "" {
		return
	}

	sessionid, _ = url.QueryUnescape(cookie.Value)

	if session, ok := p.sessions[sessionid]; ok {
		session.Value.(SessionStore).Release()
	}

	http.SetCookie(w, &http.Cookie{
		Name:     p.CookieName,
		Path:     "/",
		HttpOnly: true,
		Expires:  time.Now(),
		MaxAge:   -1})

	return
}

func (p *SessionManager) GetSessionById(sid string) (session SessionStore, err error) {
	if sid == "" {
		return nil, nil
	}

	if sess, ok := p.sessions[sid]; ok {
		session = sess.Value.(SessionStore)
		session.Active(true)
		p.lock.Lock()
		p.list.MoveToBack(sess)
		p.lock.Unlock()
		return
	}

	// 新会话
	session, err = newSessionStore(p.StoreType, p.StoreConfig)
	if err != nil {
		return
	}
	session.Id(sid)

	p.lock.Lock()
	p.sessions[sid] = p.list.PushBack(session)
	p.lock.Unlock()

	return
}

func (p *SessionManager) SessionDestroyById(sid string) {
	if session, ok := p.sessions[sid]; ok {
		session.Value.(SessionStore).Release()
	}
}

func (p *SessionManager) SessionUpdate(sid string) {
	if sess, ok := p.sessions[sid]; ok {
		sess.Value.(SessionStore).Active(true)
		p.lock.Lock()
		p.list.MoveToBack(sess)
		p.lock.Unlock()
		return
	}
}

func (p *SessionManager) Destroy() {
	p.sessions = nil
	p.list = nil
	p.destroied = true
}

func (p *SessionManager) SetPrepireRelease(pf PrepireReleaseFunc) {
	p.lock.Lock()
	p.prepireRelease = pf
	p.lock.Unlock()
}

func (p *SessionManager) sessionId() (string, error) {
	b := make([]byte, 24)
	n, err := rand.Read(b)
	if n != len(b) || err != nil {
		return "", fmt.Errorf("Could not successfully read from the system CSPRNG.")
	}

	return hex.EncodeToString(b), nil
}

func (p *SessionManager) gc() {
	var sleep int64 = 10

	for p.destroied == false {
		var element *list.Element

		p.lock.RLock()
		if element = p.list.Front(); element == nil {
			p.lock.RUnlock()
			break
		}

		if (element.Value.(SessionStore).Active(false) + p.IdleTime) > time.Now().Unix() {
			sleep = (element.Value.(SessionStore).Active(false) + int64(p.IdleTime)) - time.Now().Unix()
			p.lock.RUnlock()
			break
		}
		p.lock.RUnlock()

		log.Warningln("sessionrelease:", element.Value.(SessionStore).Id(""))
		p.lock.Lock()
		if p.prepireRelease != nil {
			p.prepireRelease(element.Value.(SessionStore))
		}
		element.Value.(SessionStore).Release()
		delete(p.sessions, element.Value.(SessionStore).Id(""))
		p.list.Remove(element)
		p.lock.Unlock()
	}

	if p.destroied == false {
		time.AfterFunc(time.Duration(sleep)*time.Second, p.gc)
	}
}

////////////////////////////////////////////////////////////////////////////////
// 公开接口
////////////////////////////////////////////////////////////////////////////////
func InitDefaultSessionManager(conf interface{}) *SessionManager {
	if defaultSessionManager != nil {
		defaultSessionManager.Destroy()
	}

	defaultSessionManager = NewSessionManager(conf)
	return defaultSessionManager
}

func GetSession(w http.ResponseWriter, r *http.Request, sid string) (session SessionStore, err error) {
	return defaultSessionManager.GetSession(w, r, sid)
}

func GetSessionById(sid string) (session SessionStore, err error) {
	return defaultSessionManager.GetSessionById(sid)
}

func SessionDestroy(w http.ResponseWriter, r *http.Request) (sid string) {
	return defaultSessionManager.SessionDestroy(w, r)
}

func SessionDestroyById(sid string) {
	defaultSessionManager.SessionDestroyById(sid)
}

func SessionUpdate(sid string) {
	defaultSessionManager.SessionUpdate(sid)
}

func SetPrepireRelease(pf PrepireReleaseFunc) {
	defaultSessionManager.SetPrepireRelease(pf)
}
