package main

import (
	"embed"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jeessy2/ddns-go/v5/config"
	"github.com/jeessy2/ddns-go/v5/dns"
	"github.com/jeessy2/ddns-go/v5/util"
	"github.com/jeessy2/ddns-go/v5/web"
	"github.com/kardianos/service"
)

// ddns-go 版本
// ddns-go version
var versionFlag = flag.Bool("v", false, "ddns-go 版本")

// 监听地址
var listen = flag.String("l", ":9876", "监听地址")

// 更新频率(秒)
var every = flag.Int("f", 300, "同步间隔时间(秒)")

// 缓存次数
var ipCacheTimes = flag.Int("cacheTimes", 5, "间隔N次与服务商比对")

// 服务管理
var serviceType = flag.String("s", "", "服务管理, 支持install, uninstall")

// 配置文件路径
var configFilePath = flag.String("c", util.GetConfigFilePathDefault(), "自定义配置文件路径")

// Web 服务
var noWebService = flag.Bool("noweb", false, "不启动 web 服务")

// 跳过验证证书
var skipVerify = flag.Bool("skipVerify", false, "跳过验证证书, 适合不能升级的老系统")

// 自定义 DNS 服务器
var customDNSServer = flag.String("dns", "", "自定义 DNS 服务器（例如 1.1.1.1）")

//go:embed static
var staticEmbeddedFiles embed.FS

//go:embed favicon.ico
var faviconEmbeddedFile embed.FS

// version
var version = "DEV"

func main() {
	flag.Parse()
	if *versionFlag {
		fmt.Println(version)
		return
	}
	if _, err := net.ResolveTCPAddr("tcp", *listen); err != nil {
		log.Fatalf("解析监听地址异常，%s", err)
	}
	os.Setenv(web.VersionEnv, version)
	if *configFilePath != "" {
		absPath, _ := filepath.Abs(*configFilePath)
		os.Setenv(util.ConfigFilePathENV, absPath)
	}
	if *skipVerify {
		os.Setenv(util.SkipVerifyENV, "true")
	}
	if *customDNSServer != "" {
		os.Setenv(util.DNSServerEnv, *customDNSServer+":53")
	}
	os.Setenv(util.IPCacheTimesENV, strconv.Itoa(*ipCacheTimes))
	switch *serviceType {
	case "install":
		installService()
	case "uninstall":
		uninstallService()
	default:
		if util.IsRunInDocker() {
			run()
		} else {
			s := getService()
			status, _ := s.Status()
			if status != service.StatusUnknown {
				// 以服务方式运行
				s.Run()
			} else {
				// 非服务方式运行
				switch s.Platform() {
				case "windows-service":
					log.Println("可使用 .\\ddns-go.exe -s install 安装服务运行")
				default:
					log.Println("可使用 sudo ./ddns-go -s install 安装服务运行")
				}
				run()
			}
		}
	}
}

func run() {
	// 兼容v5.0.0之前的配置文件
	conf, _ := config.GetConfigCached()
	conf.CompatibleConfig()

	if !*noWebService {
		go func() {
			// 启动web服务
			err := runWebServer()
			if err != nil {
				log.Println(err)
				time.Sleep(time.Minute)
				os.Exit(1)
			}
		}()
	}

	// 定时运行
	dns.RunTimer(time.Duration(*every) * time.Second)
}

func staticFsFunc(writer http.ResponseWriter, request *http.Request) {
	http.FileServer(http.FS(staticEmbeddedFiles)).ServeHTTP(writer, request)
}

func faviconFsFunc(writer http.ResponseWriter, request *http.Request) {
	http.FileServer(http.FS(faviconEmbeddedFile)).ServeHTTP(writer, request)
}

func runWebServer() error {
	// 启动静态文件服务
	http.HandleFunc("/static/", web.BasicAuth(staticFsFunc))
	http.HandleFunc("/favicon.ico", web.BasicAuth(faviconFsFunc))

	http.HandleFunc("/", web.BasicAuth(web.Writing))
	http.HandleFunc("/save", web.BasicAuth(web.Save))
	http.HandleFunc("/logs", web.BasicAuth(web.Logs))
	http.HandleFunc("/clearLog", web.BasicAuth(web.ClearLog))
	http.HandleFunc("/ipv4NetInterface", web.BasicAuth(web.Ipv4NetInterfaces))
	http.HandleFunc("/ipv6NetInterface", web.BasicAuth(web.Ipv6NetInterfaces))
	http.HandleFunc("/webhookTest", web.BasicAuth(web.WebhookTest))

	log.Println("监听", *listen, "...")

	l, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("监听端口发生异常, 请检查端口是否被占用: %w", err)
	}

	// 没有配置, 自动打开浏览器
	autoOpenExplorer()

	return http.Serve(l, nil)
}

type program struct{}

func (p *program) Start(s service.Service) error {
	// Start should not block. Do the actual work async.
	go p.run()
	return nil
}
func (p *program) run() {
	run()
}
func (p *program) Stop(s service.Service) error {
	// Stop should not block. Return with a few seconds.
	return nil
}

func getService() service.Service {
	options := make(service.KeyValue)
	var depends []string

	// 确保服务等待网络就绪后再启动
	switch service.ChosenSystem().String() {
	case "unix-systemv":
		options["SysvScript"] = sysvScript
	case "windows-service":
		// 将 Windows 服务的启动类型设为自动(延迟启动)
		options["DelayedAutoStart"] = true
	default:
		// 向 Systemd 添加网络依赖
		depends = append(depends, "Requires=network.target",
			"After=network-online.target")
	}

	svcConfig := &service.Config{
		Name:         "ddns-go",
		DisplayName:  "ddns-go",
		Description:  "简单好用的DDNS。自动更新域名解析到公网IP(支持阿里云、腾讯云dnspod、Cloudflare、Callback、华为云、百度云、Porkbun、GoDaddy、Google Domain)",
		Arguments:    []string{"-l", *listen, "-f", strconv.Itoa(*every), "-c", *configFilePath},
		Dependencies: depends,
		Option:       options,
	}

	if *noWebService {
		svcConfig.Arguments = append(svcConfig.Arguments, "-noweb")
	}

	if *skipVerify {
		svcConfig.Arguments = append(svcConfig.Arguments, "-skipVerify")
	}

	prg := &program{}
	s, err := service.New(prg, svcConfig)
	if err != nil {
		log.Fatalln(err)
	}
	return s
}

// 卸载服务
func uninstallService() {
	s := getService()
	s.Stop()
	if service.ChosenSystem().String() == "unix-systemv" {
		if _, err := exec.Command("/etc/init.d/ddns-go", "stop").Output(); err != nil {
			log.Println(err)
		}
	}
	if err := s.Uninstall(); err == nil {
		log.Println("ddns-go 服务卸载成功!")
	} else {
		log.Printf("ddns-go 服务卸载失败, ERR: %s\n", err)
	}
}

// 安装服务
func installService() {
	s := getService()

	status, err := s.Status()
	if err != nil && status == service.StatusUnknown {
		// 服务未知，创建服务
		if err = s.Install(); err == nil {
			s.Start()
			log.Println("安装 ddns-go 服务成功! 请打开浏览器并进行配置。")
			if service.ChosenSystem().String() == "unix-systemv" {
				if _, err := exec.Command("/etc/init.d/ddns-go", "enable").Output(); err != nil {
					log.Println(err)
				}
				if _, err := exec.Command("/etc/init.d/ddns-go", "start").Output(); err != nil {
					log.Println(err)
				}
			}
			return
		}

		log.Printf("安装 ddns-go 服务失败, ERR: %s\n", err)
	}

	if status != service.StatusUnknown {
		log.Println("ddns-go 服务已安装, 无需再次安装")
	}
}

// 打开浏览器
func autoOpenExplorer() {
	_, err := config.GetConfigCached()
	// 未找到配置文件
	if err != nil {
		if util.IsRunInDocker() {
			// docker中运行, 提示
			fmt.Println("Docker中运行, 请在浏览器中打开 http://docker主机IP:端口 进行配置")
		} else {
			// 主机运行, 打开浏览器
			addr, err := net.ResolveTCPAddr("tcp", *listen)
			if err != nil {
				return
			}
			url := fmt.Sprintf("http://127.0.0.1:%d", addr.Port)
			if addr.IP.IsGlobalUnicast() {
				url = fmt.Sprintf("http://%s", addr.String())
			}
			go util.OpenExplorer(url)
		}
	}
}

const sysvScript = `#!/bin/sh /etc/rc.common
DESCRIPTION="{{.Description}}"
cmd="{{.Path}}{{range .Arguments}} {{.|cmd}}{{end}}"
name="ddns-go"
pid_file="/var/run/$name.pid"
stdout_log="/var/log/$name.log"
stderr_log="/var/log/$name.err"
START=99
get_pid() {
    cat "$pid_file"
}
is_running() {
    [ -f "$pid_file" ] && cat /proc/$(get_pid)/stat > /dev/null 2>&1
}
start() {
	if is_running; then
		echo "Already started"
	else
		echo "Starting $name"
		{{if .WorkingDirectory}}cd '{{.WorkingDirectory}}'{{end}}
		$cmd >> "$stdout_log" 2>> "$stderr_log" &
		echo $! > "$pid_file"
		if ! is_running; then
			echo "Unable to start, see $stdout_log and $stderr_log"
			exit 1
		fi
	fi
}
stop() {
	if is_running; then
		echo -n "Stopping $name.."
		kill $(get_pid)
		for i in $(seq 1 10)
		do
			if ! is_running; then
				break
			fi
			echo -n "."
			sleep 1
		done
		echo
		if is_running; then
			echo "Not stopped; may still be shutting down or shutdown may have failed"
			exit 1
		else
			echo "Stopped"
			if [ -f "$pid_file" ]; then
				rm "$pid_file"
			fi
		fi
	else
		echo "Not running"
	fi
}
restart() {
	stop
	if is_running; then
		echo "Unable to stop, will not attempt to start"
		exit 1
	fi
	start
}
`
