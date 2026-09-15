//go:build windows

package main

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHasTrayArg(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"-tray"}, true},
		{[]string{"--tray"}, true},
		{[]string{"-TRAY"}, true},
		{[]string{"-service", "-config", `D:\lanet-node\lanet.json`}, false},
		{[]string{"-autorun"}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := hasTrayArg(c.args); got != c.want {
			t.Errorf("hasTrayArg(%v) = %v，期望 %v", c.args, got, c.want)
		}
	}
}

func TestNormalizeConsoleURL(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"127.0.0.1:8900", "http://127.0.0.1:8900"},
		{"0.0.0.0:8900", "http://127.0.0.1:8900"}, // 通配监听地址不能直接连
		{"http://0.0.0.0:8900", "http://127.0.0.1:8900"},
		{"http://[::]:8900", "http://127.0.0.1:8900"},
		{"10.0.0.5", "http://10.0.0.5:8900"},               // 缺端口补默认
		{"http://127.0.0.1:8901", "http://127.0.0.1:8901"}, // 端口回退后的值原样保留
		{"", ""},
		{"-", ""},
	}
	for _, c := range cases {
		if got := normalizeConsoleURL(c.raw); got != c.want {
			t.Errorf("normalizeConsoleURL(%q) = %q，期望 %q", c.raw, got, c.want)
		}
	}
}

func TestSwapPort(t *testing.T) {
	if got := swapPort("http://127.0.0.1:8900", 8901); got != "http://127.0.0.1:8901" {
		t.Errorf("swapPort = %q，期望 http://127.0.0.1:8901", got)
	}
	if got := swapPort("不是URL", 8901); got != "" {
		t.Errorf("非法输入应返回空串，实际 %q", got)
	}
}

// TestTrayTaskXMLShape 锁住托盘任务定义的关键声明。这三条任何一条错了，
// 图标都不会出现或会弹 UAC：
//   - InteractiveToken：进程进用户会话（Session 0 里画不出托盘）；
//   - HighestAvailable：exe 内嵌 requireAdministrator 清单，由任务计划程序
//     代为提权，用户不会看到 UAC 弹窗；
//   - 命令行必须带 -tray：否则任务拉起来的是完整节点，会和服务抢 TUN/端口。
func TestTrayTaskXMLShape(t *testing.T) {
	doc, err := trayTaskXML()
	if err != nil {
		t.Fatalf("生成任务定义失败: %v", err)
	}
	musts := []string{
		`<?xml version="1.0" encoding="UTF-16"?>`,
		"<LogonType>InteractiveToken</LogonType>",
		"<RunLevel>HighestAvailable</RunLevel>",
		"<LogonTrigger>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
		"-tray -config",
	}
	for _, m := range musts {
		if !strings.Contains(doc, m) {
			t.Errorf("任务定义缺少 %q", m)
		}
	}
	// 必须是 well-formed XML，否则 schtasks /Create /XML 直接拒绝。
	// 声明写的是 UTF-16（schtasks 只接受 UTF-16 文件，落盘前手工编码过），
	// 而 Go 标准库不内置 UTF-16 解码，所以给一个透传的 CharsetReader——
	// doc 本身就是文本，这里只是让解析器别在声明处卡住。
	var v struct {
		XMLName xml.Name
	}
	dec := xml.NewDecoder(strings.NewReader(doc))
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("任务定义不是合法 XML: %v", err)
	}
	if v.XMLName.Local != "Task" {
		t.Errorf("根元素应为 Task，实际 %q", v.XMLName.Local)
	}
}

// TestQueryTrayStatusParsesState 校验 /api/state 的解析：成员数 / 在线数 /
// 待审批数 / 本机虚拟 IP 都要落到菜单上。
func TestQueryTrayStatusParsesState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/state" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"info":{"name":"anyang-job-pc","virtual_ip":"10.7.207.102"},
			"members":[{"online":true},{"online":false},{"online":true}],
			"pending_count":2
		}`))
	}))
	defer srv.Close()

	st := queryTrayStatus(newTrayClient(), trayEndpoint{url: srv.URL})
	if !st.reachable {
		t.Fatalf("节点应判定为可达: %+v", st)
	}
	if st.name != "anyang-job-pc" || st.virtualIP != "10.7.207.102" {
		t.Errorf("身份解析错误: name=%q ip=%q", st.name, st.virtualIP)
	}
	if st.members != 3 || st.online != 2 {
		t.Errorf("成员统计错误: members=%d online=%d，期望 3/2", st.members, st.online)
	}
	if st.pending != 2 {
		t.Errorf("待审批数错误: %d，期望 2", st.pending)
	}
}

// TestQueryTrayStatusNodeDown 节点没在跑时必须安静地报「不可达」，
// 不能抛错、不能让托盘卡住。
func TestQueryTrayStatusNodeDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	st := queryTrayStatus(newTrayClient(), trayEndpoint{url: srv.URL})
	if st.reachable {
		t.Errorf("5xx 不应判定为可达: %+v", st)
	}
	if st.errMsg == "" {
		t.Error("不可达时应带原因，便于排查")
	}
}

// TestQueryTrayStatusLogsIn 控制台设了访问密码时，托盘要用配置里的密码换
// 会话 Cookie 再取状态——否则菜单永远是「节点未运行」。
func TestQueryTrayStatusLogsIn(t *testing.T) {
	// 控制台的会话 Cookie 名（sdk/go/lanet 的 sessionCookieName 跨包不可见，
	// 这里按协议字面量对齐；托盘侧靠 cookiejar 自动回带，不关心名字）。
	const cookieName = "lanet_console_session"
	var loginCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			_ = r.ParseForm()
			if r.FormValue("password") != "secret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "tok", Path: "/"})
			loginCalled = true
		case "/api/state":
			ck, err := r.Cookie(cookieName)
			if err != nil || ck.Value != "tok" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"未登录或会话已过期"}`))
				return
			}
			_, _ = w.Write([]byte(`{"info":{"name":"n","virtual_ip":"10.7.1.2"},"members":[],"pending_count":0}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	st := queryTrayStatus(newTrayClient(), trayEndpoint{url: srv.URL, password: "secret"})
	if !loginCalled {
		t.Fatal("遇到 401 应先登录再重试")
	}
	if !st.reachable {
		t.Fatalf("登录后应能取到状态: %+v", st)
	}
	if st.virtualIP != "10.7.1.2" {
		t.Errorf("虚拟 IP 解析错误: %q", st.virtualIP)
	}
}

// TestNormalizeConsoleURLFromConfigFile 配置文件里写 0.0.0.0 时（VPS 容器常见），
// 托盘要把它换成 127.0.0.1 才连得上。
func TestNormalizeConsoleURLFromConfigFile(t *testing.T) {
	if got := normalizeConsoleURL("0.0.0.0:8900"); !strings.HasPrefix(got, "http://127.0.0.1:") {
		t.Errorf("0.0.0.0 应换成回环地址，实际 %q", got)
	}
}
