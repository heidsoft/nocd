/*
 * Copyright (c) 2018, 奶爸<1@5.nu>
 * All rights reserved.
 */

package ssh

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"golang.org/x/crypto/ssh"
	"net"
	"strings"
	"time"

	"github.com/naiba/nocd"
	"github.com/pkg/errors"
	"io"
)

//GenKeyPair 创建密钥对
func GenKeyPair() (string, string, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}

	privateKeyPEM := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)}
	var private bytes.Buffer
	if err := pem.Encode(&private, privateKeyPEM); err != nil {
		return "", "", err
	}

	// generate public key
	pub, err := ssh.NewPublicKey(&privateKey.PublicKey)
	if err != nil {
		return "", "", err
	}

	public := ssh.MarshalAuthorizedKey(pub)
	return string(public), private.String(), nil
}

//CheckLogin 检查服务器是否存在
func CheckLogin(address string, port int, privateKey string, login string) error {
	conn, err := dial(address, login, privateKey, port)
	if err != nil {
		nocd.Logger().Infoln("ssh.CheckLogin", err)
		return errors.New("连接服务器失败")
	}
	defer conn.Close()
	session, err := conn.NewSession()
	if err != nil {
		return errors.New("建立会话失败")
	}
	defer session.Close()
	opt, err := session.Output("whoami")
	if strings.TrimSpace(string(opt)) != login {
		nocd.Logger().Infoln(string(opt) + "|" + err.Error())
		return errors.New("用户名验证失败")
	}
	return nil
}

//Deploy 进行部署
func Deploy(pipeline nocd.Pipeline, log *nocd.PipeLog) {

	nocd.Logger().Debugln(log.ID, " deploy start")

	start := time.Now()
	pr, pw := io.Pipe()
	defer func() {
		pr.Close()
		pw.Close()
	}()
	run := &nocd.Running{
		Finish:     make(chan bool, 1),
		Log:        log,
		RunningLog: make([]string, 0),
	}
	nocd.RunningLogs[log.ID] = run

	defer func() {
		close(run.Finish)
		delete(nocd.RunningLogs, log.ID)
		run.Log.Log = strings.Join(run.RunningLog, "\n")
		// 保留最后8000字
		if len(run.Log.Log) > 8000 {
			run.Log.Log = run.Log.Log[:3998] + "...." + run.Log.Log[len(run.Log.Log)-3998:]
		}
		run.Log.StoppedAt = time.Now()
		nocd.Logger().Debugln(log.ID, " deploy stop")
	}()

	conn, err := dial(pipeline.Server.Address, pipeline.Server.Login, pipeline.User.PrivateKey, pipeline.Server.Port)
	if err != nil {
		nocd.Logger().Debugln(err)
		run.Log.Status = nocd.PipeLogStatusErrorServerConn
		return
	}
	defer conn.Close()

	session, err := conn.NewSession()
	if err != nil {
		run.Log.Status = nocd.PipeLogStatusErrorServerConn
		return
	}
	defer session.Close()

	timer := time.NewTimer(time.Minute * 30)
	stderr := new(bytes.Buffer)
	session.Stdout = pw
	session.Stderr = stderr

	go func() {
		err = session.Start(pipeline.Shell)
		if err != nil {
			nocd.Logger().Debugln(err)
			run.Log.Status = nocd.PipeLogStatusErrorShellExec
			run.RunningLog = append(run.RunningLog, appendLog(start)+stderr.String())
			run.RunningLog = append(run.RunningLog, appendLog(start)+err.Error())
			run.Finish <- true
			return
		}
		go func() {
			old := ""
			for {
				// 已经关闭
				if run.Closed {
					return
				}
				b := make([]byte, 10)
				num, err := pr.Read(b)
				if err != nil {
					if run.Closed {
						return
					}
					if err == io.ErrClosedPipe {
						run.Log.Status = nocd.PipeLogStatusSuccess
					} else {
						nocd.Logger().Debugln(err)
						run.Log.Status = nocd.PipeLogStatusErrorShellExec
						run.RunningLog = append(run.RunningLog, appendLog(start)+stderr.String())
					}
					run.Finish <- true
					break
				}
				newLine := b[num-1] == '\n'
				s := strings.Split(old+string(b[:num]), "\n")
				old = ""
				for i := 0; i < len(s); i++ {
					if i == len(s)-1 && !newLine {
						old = s[i]
						break
					}
					run.RunningLog = append(run.RunningLog, appendLog(start)+s[i])
				}
			}
		}()
		err = session.Wait()
		if run.Closed {
			return
		}
		if err != nil {
			nocd.Logger().Debugln(err)
			run.Log.Status = nocd.PipeLogStatusErrorShellExec
			run.RunningLog = append(run.RunningLog, appendLog(start)+stderr.String())
			run.RunningLog = append(run.RunningLog, appendLog(start)+err.Error())
		}
		pw.Close()
		pr.Close()
	}()

	select {
	case <-timer.C:
		run.Closed = true
		run.Log.Status = nocd.PipeLogStatusErrorTimeout
		return
	case <-run.Finish:
		run.Closed = true
		return
	}
}

func appendLog(start time.Time) string {
	num := time.Now().Sub(start).Seconds()
	return fmt.Sprintf("%02.f", num/60/60) + ":" + fmt.Sprintf("%02d", int(num)%360/60) + ":" + fmt.Sprintf("%02d", int(num)%60) + "#"
}

func dial(address, user, pk string, port int) (*ssh.Client, error) {
	privateKey, err := ssh.ParsePrivateKey([]byte(pk))
	if err != nil {
		nocd.Logger().Debug(err, pk)
		return nil, errors.New("解析用户私钥失败")
	}
	return ssh.Dial("tcp", fmt.Sprintf("%s:%d", address, port), &ssh.ClientConfig{
		User:    user,
		Timeout: time.Second * 10,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(privateKey),
		},
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			return nil
		},
	})
}
