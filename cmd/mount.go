/*
 * JuiceFS, Copyright 2020 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/juicedata/juicefs/pkg/object"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/urfave/cli/v2"

	"github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/metric"
	"github.com/juicedata/juicefs/pkg/usage"
	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/juicedata/juicefs/pkg/version"
	"github.com/juicedata/juicefs/pkg/vfs"
)

func cmdMount() *cli.Command {
	compoundFlags := [][]cli.Flag{
		mount_flags(),
		clientFlags(),
		shareInfoFlags(),
	}
	return &cli.Command{
		Name:      "mount",
		Action:    mount,
		Category:  "SERVICE",
		Usage:     "Mount a volume",
		ArgsUsage: "META-URL MOUNTPOINT",
		Description: `
Mount the target volume at the mount point.

Examples:
# Mount in foreground
$ juicefs mount redis://localhost /mnt/jfs

# Mount in background with password protected Redis
$ juicefs mount redis://:mypassword@localhost /mnt/jfs -d
# A safer alternative
$ META_PASSWORD=mypassword juicefs mount redis://localhost /mnt/jfs -d

# Mount with a sub-directory as root
$ juicefs mount redis://localhost /mnt/jfs --subdir /dir/in/jfs

# Enable "writeback" mode, which improves performance at the risk of losing objects
$ juicefs mount redis://localhost /mnt/jfs -d --writeback

# Enable "read-only" mode
$ juicefs mount redis://localhost /mnt/jfs -d --read-only

# Disable metadata backup
$ juicefs mount redis://localhost /mnt/jfs --backup-meta 0`,
		Flags: expandFlags(compoundFlags),
	}
}

func installHandler(mp string) {
	// Go will catch all the signals
	signal.Ignore(syscall.SIGPIPE)
	signalChan := make(chan os.Signal, 10)
	signal.Notify(signalChan, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		for {
			<-signalChan
			go func() { _ = doUmount(mp, true) }()
			go func() {
				time.Sleep(time.Second * 3)
				os.Exit(1)
			}()
		}
	}()
}

func exposeMetrics(c *cli.Context, m meta.Meta, registerer prometheus.Registerer, registry *prometheus.Registry) string {
	var ip, port string
	//default set
	ip, port, err := net.SplitHostPort(c.String("metrics"))
	if err != nil {
		logger.Fatalf("metrics format error: %v", err)
	}

	meta.InitMetrics(registerer)
	vfs.InitMetrics(registerer)
	go metric.UpdateMetrics(m, registerer)
	http.Handle("/metrics", promhttp.HandlerFor(
		registry,
		promhttp.HandlerOpts{
			// Opt into OpenMetrics to support exemplars.
			EnableOpenMetrics: true,
		},
	))
	registerer.MustRegister(prometheus.NewBuildInfoCollector())

	// If not set metrics addr,the port will be auto set
	if !c.IsSet("metrics") {
		// If only set consul, ip will auto set
		if c.IsSet("consul") {
			ip, err = utils.GetLocalIp(c.String("consul"))
			if err != nil {
				logger.Errorf("Get local ip failed: %v", err)
				return ""
			}
		}
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(ip, port))
	if err != nil {
		// Don't try other ports on metrics set but listen failed
		if c.IsSet("metrics") {
			logger.Errorf("listen on %s:%s failed: %v", ip, port, err)
			return ""
		}
		// Listen port on 0 will auto listen on a free port
		ln, err = net.Listen("tcp", net.JoinHostPort(ip, "0"))
		if err != nil {
			logger.Errorf("Listen failed: %v", err)
			return ""
		}
	}

	go func() {
		if err := http.Serve(ln, nil); err != nil {
			logger.Errorf("Serve for metrics: %s", err)
		}
	}()

	metricsAddr := ln.Addr().String()
	logger.Infof("Prometheus metrics listening on %s", metricsAddr)
	return metricsAddr
}

func wrapRegister(mp, name string) (prometheus.Registerer, *prometheus.Registry) {
	registry := prometheus.NewRegistry() // replace default so only JuiceFS metrics are exposed
	registerer := prometheus.WrapRegistererWithPrefix("juicefs_",
		prometheus.WrapRegistererWith(prometheus.Labels{"mp": mp, "vol_name": name}, registry))
	registerer.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	registerer.MustRegister(prometheus.NewGoCollector())
	return registerer, registry
}

func getFormat(c *cli.Context, metaCli meta.Meta) *meta.Format {
	format, err := metaCli.Load(true)
	if err != nil {
		logger.Fatalf("load setting: %s", err)
	}
	if c.IsSet("bucket") {
		format.Bucket = c.String("bucket")
	}
	return format
}

func daemonRun(c *cli.Context, addr string, vfsConf *vfs.Config, m meta.Meta) {
	if runtime.GOOS != "windows" {
		d := c.String("cache-dir")
		if d != "memory" && !strings.HasPrefix(d, "/") {
			ad, err := filepath.Abs(d)
			if err != nil {
				logger.Fatalf("cache-dir should be absolute path in daemon mode")
			} else {
				for i, a := range os.Args {
					if a == d || a == "--cache-dir="+d {
						os.Args[i] = a[:len(a)-len(d)] + ad
					}
				}
			}
		}
	}
	sqliteScheme := "sqlite3://"
	if strings.HasPrefix(addr, sqliteScheme) {
		path := addr[len(sqliteScheme):]
		path2, err := filepath.Abs(path)
		if err == nil && path2 != path {
			for i, a := range os.Args {
				if a == addr {
					os.Args[i] = sqliteScheme + path2
				}
			}
		}
	}
	// The default log to syslog is only in daemon mode.
	utils.InitLoggers(!c.Bool("no-syslog"))
	err := makeDaemon(c, vfsConf.Format.Name, vfsConf.Meta.MountPoint, m)
	if err != nil {
		logger.Fatalf("Failed to make daemon: %s", err)
	}
}

func getVfsConf(c *cli.Context, metaConf *meta.Config, format *meta.Format, chunkConf *chunk.Config) *vfs.Config {
	return &vfs.Config{
		Meta:       metaConf,
		Format:     format,
		Version:    version.Version(),
		Chunk:      chunkConf,
		BackupMeta: c.Duration("backup-meta"),
	}
}

func registerMetaMsg(m meta.Meta, store chunk.ChunkStore, chunkConf *chunk.Config) {
	m.OnMsg(meta.DeleteChunk, func(args ...interface{}) error {
		return store.Remove(args[0].(uint64), int(args[1].(uint32)))
	})
	m.OnMsg(meta.CompactChunk, func(args ...interface{}) error {
		return vfs.Compact(*chunkConf, store, args[0].([]meta.Slice), args[1].(uint64))
	})
}

func prepareMp(mp string) {
	fi, err := os.Stat(mp)
	if !strings.Contains(mp, ":") && err != nil {
		if err := os.MkdirAll(mp, 0777); err != nil {
			if os.IsExist(err) {
				// a broken mount point, umount it
				_ = doUmount(mp, true)
			} else {
				logger.Fatalf("create %s: %s", mp, err)
			}
		}
	} else if err == nil {
		ino, _ := utils.GetFileInode(mp)
		if ino <= 1 && fi.Size() == 0 {
			// a broken mount point, umount it
			_ = doUmount(mp, true)
		}
	}
}

func getMetaConf(c *cli.Context, mp string, readOnly bool) *meta.Config {
	return &meta.Config{
		Retries:    10,
		Strict:     true,
		ReadOnly:   readOnly,
		NoBGJob:    c.Bool("no-bgjob"),
		OpenCache:  time.Duration(c.Float64("open-cache") * 1e9),
		MountPoint: mp,
		Subdir:     c.String("subdir"),
		MaxDeletes: c.Int("max-deletes"),
	}
}

func newStore(format *meta.Format, chunkConf *chunk.Config, registerer prometheus.Registerer) (object.ObjectStorage, chunk.ChunkStore) {
	blob, err := createStorage(*format)
	if err != nil {
		logger.Fatalf("object storage: %s", err)
	}
	logger.Infof("Data use %s", blob)
	store := chunk.NewCachedStore(blob, *chunkConf, registerer)
	return blob, store
}

func getChunkConf(c *cli.Context, format *meta.Format) *chunk.Config {
	chunkConf := &chunk.Config{
		BlockSize: format.BlockSize * 1024,
		Compress:  format.Compression,

		GetTimeout:    time.Second * time.Duration(c.Int("get-timeout")),
		PutTimeout:    time.Second * time.Duration(c.Int("put-timeout")),
		MaxUpload:     c.Int("max-uploads"),
		Writeback:     c.Bool("writeback"),
		Prefetch:      c.Int("prefetch"),
		BufferSize:    c.Int("buffer-size") << 20,
		UploadLimit:   c.Int64("upload-limit") * 1e6 / 8,
		DownloadLimit: c.Int64("download-limit") * 1e6 / 8,

		CacheDir:       c.String("cache-dir"),
		CacheSize:      int64(c.Int("cache-size")),
		FreeSpace:      float32(c.Float64("free-space-ratio")),
		CacheMode:      os.FileMode(0600),
		CacheFullBlock: !c.Bool("cache-partial-only"),
		AutoCreate:     true,
	}

	if chunkConf.CacheDir != "memory" {
		ds := utils.SplitDir(chunkConf.CacheDir)
		for i := range ds {
			ds[i] = filepath.Join(ds[i], format.UUID)
		}
		chunkConf.CacheDir = strings.Join(ds, string(os.PathListSeparator))
	}
	return chunkConf
}

func initBackgroundTasks(c *cli.Context, vfsConf *vfs.Config, metaConf *meta.Config, m meta.Meta, blob object.ObjectStorage, registerer prometheus.Registerer, registry *prometheus.Registry) {
	metricsAddr := exposeMetrics(c, m, registerer, registry)
	if c.IsSet("consul") {
		metric.RegisterToConsul(c.String("consul"), metricsAddr, vfsConf.Meta.MountPoint)
	}
	if !metaConf.ReadOnly && !metaConf.NoBGJob && vfsConf.BackupMeta > 0 {
		go vfs.Backup(m, blob, vfsConf.BackupMeta)
	}
	if !c.Bool("no-usage-report") {
		go usage.ReportUsage(m, version.Version())
	}
}

func mount(c *cli.Context) error {
	setup(c, 2)
	addr := c.Args().Get(0)
	mp := c.Args().Get(1)

	prepareMp(mp)
	var readOnly = c.Bool("read-only")
	for _, o := range strings.Split(c.String("o"), ",") {
		if o == "ro" {
			readOnly = true
		}
	}
	metaConf := getMetaConf(c, mp, readOnly)
	metaConf.CaseInsensi = strings.HasSuffix(mp, ":") && runtime.GOOS == "windows"
	metaCli := meta.NewClient(addr, metaConf)
	format := getFormat(c, metaCli)

	// Wrap the default registry, all prometheus.MustRegister() calls should be afterwards
	registerer, registry := wrapRegister(mp, format.Name)

	if !c.Bool("writeback") && c.IsSet("upload-delay") {
		logger.Warnf("delayed upload only work in writeback mode")
	}

	chunkConf := getChunkConf(c, format)
	chunkConf.UploadDelay = c.Duration("upload-delay")

	blob, store := newStore(format, chunkConf, registerer)
	registerMetaMsg(metaCli, store, chunkConf)

	vfsConf := getVfsConf(c, metaConf, format, chunkConf)

	if c.Bool("background") && os.Getenv("JFS_FOREGROUND") == "" {
		daemonRun(c, addr, vfsConf, metaCli)
	} else {
		go checkMountpoint(vfsConf.Format.Name, mp)
	}

	removePassword(addr)
	err := metaCli.NewSession()
	if err != nil {
		logger.Fatalf("new session: %s", err)
	}

	installHandler(mp)
	v := vfs.NewVFS(vfsConf, metaCli, store, registerer, registry)
	initBackgroundTasks(c, vfsConf, metaConf, metaCli, blob, registerer, registry)
	mount_main(v, c)
	return metaCli.CloseSession()
}
