// Copyright 2013 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lxc

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/globocom/config"
	"github.com/globocom/tsuru/db"
	"github.com/globocom/tsuru/log"
	"github.com/globocom/tsuru/provision"
	"github.com/globocom/tsuru/repository"
	"github.com/globocom/tsuru/router"
	_ "github.com/globocom/tsuru/router/nginx"
	"io"
	"labix.org/v2/mgo"
	"labix.org/v2/mgo/bson"
	"os/exec"
)

func init() {
	provision.Register("lxc", &LocalProvisioner{})
}

type LocalProvisioner struct{}

func (p *LocalProvisioner) router() (router.Router, error) {
	r, err := config.GetString("router")
	if err != nil {
		return nil, err
	}
	return router.Get(r)
}

func (p *LocalProvisioner) setup(ip, framework string) error {
	formulasPath, err := config.GetString("lxc:formulas-path")
	if err != nil {
		return err
	}
	log.Printf("Creating hooks dir for %s", ip)
	cmd := exec.Command("ssh", "-q", "-o", "StrictHostKeyChecking no", "-l", "ubuntu", ip, "sudo mkdir -p /var/lib/tsuru/hooks")
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("error on creating hooks dir for %s", ip)
		log.Print(string(output))
		log.Print(err)
		return err
	}
	log.Printf("Permissons on hooks dir for %s", ip)
	cmd = exec.Command("ssh", "-q", "-o", "StrictHostKeyChecking no", "-l", "ubuntu", ip, "sudo chown -R ubuntu /var/lib/tsuru/hooks")
	output, err = cmd.CombinedOutput()
	if err != nil {
		log.Printf("error on permissions for %s", ip)
		log.Print(string(output))
		log.Print(err)
		return err
	}
	log.Printf("coping hooks to %s", ip)
	cmd = exec.Command("scp", "-q", "-o", "StrictHostKeyChecking no", "-r", formulasPath+"/"+framework+"/hooks", "ubuntu@"+ip+":/var/lib/tsuru")
	output, err = cmd.CombinedOutput()
	if err != nil {
		log.Printf("error on execute scp with the args: %#v", cmd.Args)
		log.Print(string(output))
		log.Print(err)
		return err
	}
	return nil
}

func (p *LocalProvisioner) install(ip string) error {
	log.Printf("executing the install hook for %s", ip)
	cmd := exec.Command("ssh", "-q", "-o", "StrictHostKeyChecking no", "-l", "ubuntu", ip, "sudo /var/lib/tsuru/hooks/install")
	err := cmd.Run()
	if err != nil {
		log.Printf("error on install for %s", ip)
		log.Print(err)
		return err
	}
	return nil
}

func (p *LocalProvisioner) start(ip string) error {
	cmd := exec.Command("ssh", "-q", "-o", "StrictHostKeyChecking no", "-l", "ubuntu", ip, "sudo /var/lib/tsuru/hooks/start")
	return cmd.Run()
}

func (p *LocalProvisioner) Provision(app provision.App) error {
	go func(p *LocalProvisioner, app provision.App) {
		c := container{name: app.GetName()}
		log.Printf("creating container %s", c.name)
		u := provision.Unit{
			Name:       app.GetName(),
			AppName:    app.GetName(),
			Type:       app.GetFramework(),
			Machine:    0,
			InstanceId: app.GetName(),
			Status:     provision.StatusCreating,
			Ip:         "",
		}
		log.Printf("inserting container unit %s in the database", app.GetName())
		err := p.collection().Insert(u)
		if err != nil {
			log.Print(err)
		}
		err = c.create()
		if err != nil {
			log.Printf("error on create container %s", app.GetName())
			log.Print(err)
		}
		err = c.start()
		if err != nil {
			log.Printf("error on start container %s", app.GetName())
			log.Print(err)
		}
		u.Ip = c.Ip()
		u.Status = provision.StatusInstalling
		err = p.collection().Update(bson.M{"name": u.Name}, u)
		if err != nil {
			log.Print(err)
		}
		err = c.waitForNetwork()
		if err != nil {
			log.Print(err)
		}
		err = p.setup(c.Ip(), app.GetFramework())
		if err != nil {
			log.Printf("error on setup container %s", app.GetName())
			log.Print(err)
		}
		err = p.install(c.Ip())
		if err != nil {
			log.Printf("error on install container %s", app.GetName())
			log.Print(err)
		}
		err = p.start(c.Ip())
		if err != nil {
			log.Printf("error on start app for container %s", app.GetName())
			log.Print(err)
		}
		r, err := p.router()
		if err != nil {
			log.Print(err)
			return
		}
		err = r.AddRoute(app.GetName(), c.Ip())
		if err != nil {
			log.Printf("error on add route for %s with ip %s", app.GetName(), c.Ip())
			log.Print(err)
		}
		u.Status = provision.StatusStarted
		err = p.collection().Update(bson.M{"name": u.Name}, u)
		if err != nil {
			log.Print(err)
		}
	}(p, app)
	return nil
}

func (p *LocalProvisioner) Restart(app provision.App) error {
	var buf bytes.Buffer
	err := p.ExecuteCommand(&buf, &buf, app, "/var/lib/tsuru/hooks/restart")
	if err != nil {
		msg := fmt.Sprintf("Failed to restart the app (%s): %s", err, buf.String())
		app.Log(msg, "tsuru-provisioner")
		return &provision.Error{Reason: buf.String(), Err: err}
	}
	return nil
}

func (p *LocalProvisioner) Deploy(a provision.App, w io.Writer) error {
	if err := write(w, []byte("\n ---> Tsuru receiving push\n")); err != nil {
		return err
	}
	if err := write(w, []byte("\n ---> Replicating the application repository across units\n")); err != nil {
		return err
	}
	out, err := repository.CloneOrPull(a) // should iterate over the machines (?)
	if err != nil {
		msg := fmt.Sprintf("Got error while clonning/pulling repository: %s -- \n%s", err.Error(), string(out))
		return errors.New(msg)
	}
	if err := write(w, out); err != nil {
		return err
	}
	if err := write(w, []byte("\n ---> Installing dependencies\n")); err != nil {
		return err
	}
	if err := a.InstallDeps(w); err != nil {
		return err
	}
	if err := a.Restart(w); err != nil {
		return err
	}
	return write(w, []byte("\n ---> Deploy done!\n\n"))
}

func (p *LocalProvisioner) Destroy(app provision.App) error {
	c := container{name: app.GetName()}
	go func(c container) {
		log.Printf("stoping container %s", c.name)
		c.stop()
		log.Printf("destroying container %s", c.name)
		c.destroy()
		log.Printf("removing container %s from the database", c.name)
		p.collection().Remove(bson.M{"name": c.name})
	}(c)
	return nil
}

func (p *LocalProvisioner) Addr(app provision.App) (string, error) {
	r, err := p.router()
	if err != nil {
		return "", err
	}
	return r.Addr(app.GetName()), nil
}

func (*LocalProvisioner) AddUnits(app provision.App, units uint) ([]provision.Unit, error) {
	return []provision.Unit{}, nil
}

func (*LocalProvisioner) RemoveUnit(app provision.App, unitName string) error {
	return nil
}

func (*LocalProvisioner) ExecuteCommand(stdout, stderr io.Writer, app provision.App, cmd string, args ...string) error {
	arguments := []string{"-l", "ubuntu", "-q", "-o", "StrictHostKeyChecking no"}
	arguments = append(arguments, app.ProvisionUnits()[0].GetIp())
	arguments = append(arguments, cmd)
	arguments = append(arguments, args...)
	c := exec.Command("ssh", arguments...)
	c.Stdout = stdout
	c.Stderr = stderr
	err := c.Run()
	if err != nil {
		return err
	}
	return nil
}

func (p *LocalProvisioner) CollectStatus() ([]provision.Unit, error) {
	var units []provision.Unit
	err := p.collection().Find(nil).All(&units)
	if err != nil {
		return []provision.Unit{}, err
	}
	return units, nil
}

func (p *LocalProvisioner) collection() *mgo.Collection {
	name, err := config.GetString("lxc:collection")
	if err != nil {
		log.Fatalf("FATAL: %s.", err)
	}
	conn, err := db.Conn()
	if err != nil {
		log.Printf("Failed to connect to the database: %s", err)
	}
	return conn.Collection(name)
}

// write writes the given content to the given writer, and handls short writes.
//
// TODO(flaviamissi): this is the fourth implementation of this function, let's make just one.
func write(w io.Writer, content []byte) error {
	n, err := w.Write(content)
	if err != nil {
		return err
	}
	if n != len(content) {
		return io.ErrShortWrite
	}
	return nil
}