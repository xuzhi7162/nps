package main

import (
	"flag"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"xuzhi.cc/nps/lib/file"
	"xuzhi.cc/nps/lib/install"
	"xuzhi.cc/nps/lib/version"
	"xuzhi.cc/nps/server"
	"xuzhi.cc/nps/server/connection"
	"xuzhi.cc/nps/server/tool"
	"xuzhi.cc/nps/web/routers"

	"github.com/astaxie/beego"
	"github.com/astaxie/beego/logs"
	"xuzhi.cc/nps/lib/common"
	"xuzhi.cc/nps/lib/crypt"
	"xuzhi.cc/nps/lib/daemon"

	"github.com/kardianos/service"
)

var (
	level string
	ver   = flag.Bool("version", false, "show current version")
)

func main() {
	flag.Parse()
	// init log
	if *ver {
		common.PrintVersion()
		return
	}

	// beego 框架读取配置文件,加载至 beego 上下文
	if err := beego.LoadAppConfig("ini", filepath.Join(common.GetRunPath(), "conf", "nps.conf")); err != nil {
		log.Fatalln("load config file error", err.Error())
	}

	// 暂时不知道有啥用
	common.InitPProfFromFile()

	// 读取日志等级
	if level = beego.AppConfig.String("log_level"); level == "" {
		level = "7"
	}

	// 初始化日志打印工具
	logs.Reset()
	logs.EnableFuncCallDepth(true)
	logs.SetLogFuncCallDepth(3)

	// 从配置文件中获取日志输出路径
	logPath := beego.AppConfig.String("log_path")
	if logPath == "" {
		// 如果配置文件中没有配置日志输出路径,则取默认日志输出路径
		logPath = common.GetLogPath()
	}

	// 判断当前运行环境是否为 windows,如果为 windows 则需特别处理日志路径
	if common.IsWindows() {
		logPath = strings.Replace(logPath, "\\", "\\\\", -1)
	}

	// init service 初始化服务(是否安装为系统服务)
	options := make(service.KeyValue)
	svcConfig := &service.Config{
		Name:        "Nps",
		DisplayName: "nps内网穿透代理服务器",
		Description: "一款轻量级、功能强大的内网穿透代理服务器。支持tcp、udp流量转发，支持内网http代理、内网socks5代理，同时支持snappy压缩、站点保护、加密传输、多路复用、header修改等。支持web图形化管理，集成多用户模式。",
		Option:      options,
	}
	svcConfig.Arguments = append(svcConfig.Arguments, "service")
	if len(os.Args) > 1 && os.Args[1] == "service" {
		_ = logs.SetLogger(logs.AdapterFile, `{"level":`+level+`,"filename":"`+logPath+`","daily":false,"maxlines":100000,"color":true}`)
	} else {
		_ = logs.SetLogger(logs.AdapterConsole, `{"level":`+level+`,"color":true}`)
	}
	if !common.IsWindows() {
		svcConfig.Dependencies = []string{
			"Requires=network.target",
			"After=network-online.target syslog.target"}
		svcConfig.Option["SystemdScript"] = install.SystemdScript
		svcConfig.Option["SysvScript"] = install.SysvScript
	}
	prg := &nps{}
	prg.exit = make(chan struct{})
	s, err := service.New(prg, svcConfig)
	if err != nil {
		logs.Error(err, "service function disabled")
		run()
		// run without service
		wg := sync.WaitGroup{}
		wg.Add(1)
		wg.Wait()
		return
	}
	if len(os.Args) > 1 && os.Args[1] != "service" {
		switch os.Args[1] {
		case "reload":
			daemon.InitDaemon("nps", common.GetRunPath(), common.GetTmpPath())
			return
		case "install":
			// uninstall before
			_ = service.Control(s, "stop")
			_ = service.Control(s, "uninstall")

			binPath := install.InstallNps()
			svcConfig.Executable = binPath
			s, err := service.New(prg, svcConfig)
			if err != nil {
				logs.Error(err)
				return
			}
			err = service.Control(s, os.Args[1])
			if err != nil {
				logs.Error("Valid actions: %q\n%s", service.ControlAction, err.Error())
			}
			if service.Platform() == "unix-systemv" {
				logs.Info("unix-systemv service")
				confPath := "/etc/init.d/" + svcConfig.Name
				os.Symlink(confPath, "/etc/rc.d/S90"+svcConfig.Name)
				os.Symlink(confPath, "/etc/rc.d/K02"+svcConfig.Name)
			}
			return
		case "start", "restart", "stop":
			if service.Platform() == "unix-systemv" {
				logs.Info("unix-systemv service")
				cmd := exec.Command("/etc/init.d/"+svcConfig.Name, os.Args[1])
				err := cmd.Run()
				if err != nil {
					logs.Error(err)
				}
				return
			}
			err := service.Control(s, os.Args[1])
			if err != nil {
				logs.Error("Valid actions: %q\n%s", service.ControlAction, err.Error())
			}
			return
		case "uninstall":
			err := service.Control(s, os.Args[1])
			if err != nil {
				logs.Error("Valid actions: %q\n%s", service.ControlAction, err.Error())
			}
			if service.Platform() == "unix-systemv" {
				logs.Info("unix-systemv service")
				os.Remove("/etc/rc.d/S90" + svcConfig.Name)
				os.Remove("/etc/rc.d/K02" + svcConfig.Name)
			}
			return
		case "update":
			install.UpdateNps()
			return
		default:
			logs.Error("command is not support")
			return
		}
	}
	_ = s.Run()
}

type nps struct {
	exit chan struct{}
}

func (p *nps) Start(s service.Service) error {
	_, _ = s.Status()
	go p.run()
	return nil
}
func (p *nps) Stop(s service.Service) error {
	_, _ = s.Status()
	close(p.exit)
	if service.Interactive() {
		os.Exit(0)
	}
	return nil
}

func (p *nps) run() error {
	defer func() {
		if err := recover(); err != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			logs.Warning("nps: panic serving %v: %v\n%s", err, string(buf))
		}
	}()
	run()
	select {
	case <-p.exit:
		logs.Warning("stop...")
	}
	return nil
}

// run 程序启动真实入口
func run() {
	// 初始化路由
	routers.Init()

	task := &file.Tunnel{
		Mode: "webServer",
	}

	// 从配置文件中读取 bridge_port 配置项
	bridgePort, err := beego.AppConfig.Int("bridge_port")
	if err != nil {
		logs.Error("Getting bridge_port error", err)
		os.Exit(0)
	}

	logs.Info("the version of server is %s ,allow client core version to be %s", version.VERSION, version.GetVersion())

	// 初始化连接服务
	connection.InitConnectionService()

	//crypt.InitTls(filepath.Join(common.GetRunPath(), "conf", "server.pem"), filepath.Join(common.GetRunPath(), "conf", "server.key"))
	// 初始化tls
	crypt.InitTls()

	// 初始化允许连接的端口
	tool.InitAllowPort()

	// 开始系统服务
	tool.StartSystemInfo()

	// 从配置文件中获取连接超时时间
	timeout, err := beego.AppConfig.Int("disconnect_timeout")
	if err != nil {
		timeout = 60
	}

	// 协程启动新的服务
	// 该处启动的为主服务, 所有客户端连接的主端口
	go server.StartNewServer(bridgePort, task, beego.AppConfig.String("bridge_type"), timeout)
}
