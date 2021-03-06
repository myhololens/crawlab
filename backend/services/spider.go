package services

import (
	"crawlab/constants"
	"crawlab/database"
	"crawlab/entity"
	"crawlab/lib/cron"
	"crawlab/model"
	"crawlab/services/spider_handler"
	"crawlab/utils"
	"fmt"
	"github.com/apex/log"
	"github.com/globalsign/mgo"
	"github.com/globalsign/mgo/bson"
	"github.com/spf13/viper"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
)

type SpiderFileData struct {
	FileName string
	File     []byte
}

type SpiderUploadMessage struct {
	FileId   string
	FileName string
	SpiderId string
}

// 上传zip文件到GridFS
func UploadToGridFs(fileName string, filePath string) (fid bson.ObjectId, err error) {
	fid = ""

	// 获取MongoDB GridFS连接
	s, gf := database.GetGridFs("files")
	defer s.Close()

	// 创建一个新GridFS文件
	f, err := gf.Create(fileName)
	if err != nil {
		debug.PrintStack()
		return
	}

	//分片读取爬虫zip文件
	err = ReadFileByStep(filePath, WriteToGridFS, f)
	if err != nil {
		debug.PrintStack()
		return "", err
	}

	// 删除zip文件
	if err = os.Remove(filePath); err != nil {
		debug.PrintStack()
		return
	}
	// 关闭文件，提交写入
	if err = f.Close(); err != nil {
		return "", err
	}
	// 文件ID
	fid = f.Id().(bson.ObjectId)

	return fid, nil
}

// 写入grid fs
func WriteToGridFS(content []byte, f *mgo.GridFile) {
	if _, err := f.Write(content); err != nil {
		debug.PrintStack()
		return
	}
}

//分片读取大文件
func ReadFileByStep(filePath string, handle func([]byte, *mgo.GridFile), fileCreate *mgo.GridFile) error {
	f, err := os.OpenFile(filePath, os.O_RDONLY, 0777)
	if err != nil {
		log.Infof("can't opened this file")
		return err
	}
	defer utils.Close(f)
	s := make([]byte, 4096)
	for {
		switch nr, err := f.Read(s[:]); true {
		case nr < 0:
			_, _ = fmt.Fprintf(os.Stderr, "cat: error reading: %s\n", err.Error())
			debug.PrintStack()
		case nr == 0: // EOF
			return nil
		case nr > 0:
			handle(s[0:nr], fileCreate)
		}
	}
}

// 发布所有爬虫
func PublishAllSpiders() {
	// 获取爬虫列表
	spiders, _, _ := model.GetSpiderList(nil, 0, constants.Infinite)
	if len(spiders) == 0 {
		return
	}
	log.Infof("start sync spider to local, total: %d", len(spiders))
	// 遍历爬虫列表
	for _, spider := range spiders {
		// 异步发布爬虫
		go func(s model.Spider) {
			PublishSpider(s)
		}(spider)
	}
}

// 发布爬虫
func PublishSpider(spider model.Spider) {
	// 查询gf file，不存在则删除
	gfFile := model.GetGridFs(spider.FileId)
	if gfFile == nil {
		_ = model.RemoveSpider(spider.Id)
		return
	}
	spiderSync := spider_handler.SpiderSync{
		Spider: spider,
	}

	//目录不存在，则直接下载
	path := filepath.Join(viper.GetString("spider.path"), spider.Name)
	if !utils.Exists(path) {
		log.Infof("path not found: %s", path)
		spiderSync.Download()
		spiderSync.CreateMd5File(gfFile.Md5)
		return
	}
	// md5文件不存在，则下载
	md5 := filepath.Join(path, spider_handler.Md5File)
	if !utils.Exists(md5) {
		log.Infof("md5 file not found: %s", md5)
		spiderSync.RemoveSpiderFile()
		spiderSync.Download()
		spiderSync.CreateMd5File(gfFile.Md5)
		return
	}
	// md5值不一样，则下载
	md5Str := utils.ReadFileOneLine(md5)
	// 去掉空格以及换行符
	md5Str = strings.Replace(md5Str, " ", "", -1)
	md5Str = strings.Replace(md5Str, "\n", "", -1)
	if gfFile.Md5 != md5Str {
		log.Infof("md5 is different, gf-md5:%s, file-md5:%s", gfFile.Md5, md5Str)
		spiderSync.RemoveSpiderFile()
		spiderSync.Download()
		spiderSync.CreateMd5File(gfFile.Md5)
		return
	}
}

func RemoveSpider(id string) error {
	// 获取该爬虫
	spider, err := model.GetSpider(bson.ObjectIdHex(id))
	if err != nil {
		return err
	}

	// 删除爬虫文件目录
	path := filepath.Join(viper.GetString("spider.path"), spider.Name)
	utils.RemoveFiles(path)

	// 删除其他节点的爬虫目录
	msg := entity.NodeMessage{
		Type:     constants.MsgTypeRemoveSpider,
		SpiderId: id,
	}
	if err := database.Pub(constants.ChannelAllNode, msg); err != nil {
		return err
	}

	// 从数据库中删除该爬虫
	if err := model.RemoveSpider(bson.ObjectIdHex(id)); err != nil {
		return err
	}

	// 删除日志文件
	if err := RemoveLogBySpiderId(spider.Id); err != nil {
		return err
	}

	// 删除爬虫对应的task任务
	if err := model.RemoveTaskBySpiderId(spider.Id); err != nil {
		return err
	}

	// TODO 定时任务如何处理
	return nil
}

// 启动爬虫服务
func InitSpiderService() error {
	// 构造定时任务执行器
	c := cron.New(cron.WithSeconds())
	if _, err := c.AddFunc("0 * * * * *", PublishAllSpiders); err != nil {
		return err
	}
	// 启动定时任务
	c.Start()

	return nil
}
