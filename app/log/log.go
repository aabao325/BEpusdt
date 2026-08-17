package log

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/natefinch/lumberjack"
	"github.com/sirupsen/logrus"
)

var (
	Task    *logrus.Logger
	be      *logrus.Logger
	loggers []*lumberjack.Logger
	logPath string
)

func newLogger(file string) (*logrus.Logger, error) {
	logger := logrus.New()
	logger.SetFormatter(&logrus.TextFormatter{
		ForceColors:     true,
		ForceQuote:      true,
		TimestampFormat: "2006-01-02 15:04:05",
		FullTimestamp:   true,
	})

	logger.SetLevel(logrus.InfoLevel)

	// 文件轮转：单文件 50MB（降低），保留 2 个备份（约 24 小时），1 天过期
	fileWriter := &lumberjack.Logger{
		Filename:   file,
		MaxSize:    50,  // MB，从 300 降到 50
		MaxBackups: 2,   // 从 5 降到 2，约覆盖 24 小时
		MaxAge:     1,   // 从 7 天降到 1 天
		Compress:   true,
	}

	// 同时输出到文件和 stdout（docker logs 能看到）
	logger.SetOutput(io.MultiWriter(os.Stdout, fileWriter))
	loggers = append(loggers, fileWriter)

	return logger, nil
}

func Init(dir string) error {
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return fmt.Errorf("创建日志目录失败：%w", err)
	}

	var err error

	be, err = newLogger(filepath.Join(dir, "bepusdt.log"))
	if err != nil {
		return err
	}

	Task, err = newLogger(filepath.Join(dir, "task.log"))
	if err != nil {
		return err
	}

	logPath = dir

	return nil
}

func Debug(args ...interface{}) {
	be.Debugln(args...)
}

func Info(args ...interface{}) {
	be.Infoln(args...)
}

func Error(args ...interface{}) {
	be.Errorln(args...)
}

func Warn(args ...interface{}) {
	be.Warnln(args...)
}

func GetWriter() *io.PipeWriter {

	return be.Writer()
}

func GetPath() string {
	return logPath
}

func Close() {
	for _, f := range loggers {
		if f != nil {
			if err := f.Close(); err != nil {
				_, _ = fmt.Fprintln(os.Stderr, fmt.Sprintf("日志句柄资源关闭错误：%s", err.Error()))
			}
		}
	}

	loggers = nil
}
