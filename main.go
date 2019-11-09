package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PuerkitoBio/goquery"
)

var (
	ch chan int
	wg *sync.WaitGroup
)

type FileDownload struct {
	url         string
	filename    string
	worknum     int64
	size        int64 // 文件总大小
	currentSize int64 // 当前大小
	preSize     int64 // 上一次传输大小
	blockSize   int64
	err         string
	blocks      []*BlockFile
	wg          *sync.WaitGroup
	offset      int64 // 文件偏移
}

// 块文件
type BlockFile struct {
	filename string
	start    int64
	end      int64
	offset   int64 // 文件偏移
	file     *os.File
}

func NewFileDownload(urlAddress, dir string) (r *FileDownload, err error) {
	// 获取文件信息
	resp, err := http.Get(urlAddress)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	//判断是否支持多线下载
	if strings.Compare(resp.Header.Get("Accept-Ranges"), "bytes") != 0 {
		panic("nonsupport ~")
	}

	err = os.MkdirAll(dir, os.ModePerm)
	if err != nil {
		return
	}
	filename := filepath.Join(dir, filepath.Base(urlAddress))
	size := resp.ContentLength
	r = &FileDownload{
		url:      urlAddress,
		size:     size,
		blocks:   make([]*BlockFile, 0),
		wg:       new(sync.WaitGroup),
		filename: filename,
	}
	isExsit, start := isFileExit(filename, size)
	if isExsit && start < 0 {
		err = errors.New("文件已经存在，若要重新下载，请删除文件后重试")
		return
	}
	lessize := size
	if isExsit && start > 0 {
		lessize = size - start
	}

	r.worknum = 1

	switch true {
	case lessize <= 1024*1024*10:
		r.worknum = 8
		r.blockSize = lessize / 8
	case lessize > 1024*1024*10 && lessize <= 1024*1024*100: // 小于 100m
		r.worknum = 16
		r.blockSize = lessize / 8
	case lessize > 1024*1024*100 && lessize <= 1024*1024*1024: // 大于100M 小于1G
		r.worknum = 32
		r.blockSize = lessize / 16
	default:
		r.worknum = 64
		r.blockSize = 1024 * 1024 * 64 // 每块 64m
	}

	return
}

func (fd *FileDownload) download() (err error) {
	fmt.Println("start download ", fd.filename)
	go func() {
		for {
			select {
			case <-time.After(1 * time.Second):
				num := atomic.LoadInt64(&fd.currentSize) - atomic.LoadInt64(&fd.preSize)
				fd.preSize = fd.currentSize
				speed := float64(num)
				fmt.Printf("download %s ---> %s/%s %.3f%% speed:%s/s\n", fd.filename, byteFormatPrint(fd.currentSize),
					byteFormatPrint(fd.size), float64(fd.currentSize)/float64(fd.size)*100, byteFormatPrintFloat(speed))
				if fd.currentSize >= fd.size {
					return
				}
			}
		}
	}()
	f, err := os.OpenFile(fd.filename, os.O_WRONLY|os.O_CREATE, 0777)
	if err != nil {
		return
	}

	defer f.Close()

	fb := func(fdst, fscr *os.File) (err error) {
		buf := make([]byte, 4096)
		var offset int64 = 0
		defer fscr.Close()
		for {
			n, err := fscr.ReadAt(buf, offset)
			offset += int64(n)

			nw, err1 := fdst.WriteAt(buf[:n], fd.offset)
			if err1 != nil {
				return err1
			}
			fd.offset += int64(nw)
			if err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
		}
		return
	}

	finfo, _ := os.Stat(fd.filename)
	fd.offset = finfo.Size()
	fd.currentSize = fd.offset
	fd.preSize = fd.offset

	var blockSize int64 = fd.blockSize

	var begin int64 = fd.offset
	flag := false
	for begin < fd.size && !flag {

		for i := 1; i <= 32; i++ {
			if begin > fd.size {
				break
			}
			end := begin + blockSize
			if end > fd.size {
				end = fd.size
				flag = true
			}
			blockFilename := fmt.Sprintf("%s.lxwtmp-%d-%d", fd.filename, begin, end)
			fd.blocks = append(fd.blocks, &BlockFile{
				filename: blockFilename,
				start:    begin,
				end:      end,
			})
			if flag {
				break
			}
			begin = 1 + end
		}
		wg := new(sync.WaitGroup)
		for _, item := range fd.blocks {
			itemBlock := item
			wg.Add(1)
			go func(block *BlockFile) {
				defer wg.Done()
				_, err := block.startDownload(fd.url, fd)
				if err != nil {
					fd.err = err.Error()
					return
				}
				return
			}(itemBlock)

		}
		wg.Wait()
		for _, item := range fd.blocks {
			itemBlock := item
			fsrc, err := os.Open(itemBlock.filename)
			if err != nil {
				return err
			}
			err = fb(f, fsrc)
			if err != nil {
				return err
			}

			err = os.Remove(itemBlock.filename)
			if err != nil {
				fmt.Println("移除文件失败")
			}
		}
		fd.preSize = 0
		fd.blocks = make([]*BlockFile, 0)
	}

	// 校验文件
	info, _ := os.Stat(fd.filename)
	if info.Size() != fd.size {
		err = errors.New("文件未能完全下载:" + fmt.Sprintf("filesize:%d,total:%d", info.Size(), fd.size))
		return
	}
	fmt.Println("download done", fd.filename)
	return nil
}

func (bl *BlockFile) download(urlAddress string, fd *FileDownload) (err error) {
	client := &http.Client{}
	req, err := http.NewRequest("GET", urlAddress, nil)
	if err != nil {
		return
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", bl.start, bl.end))

	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		needSize := bl.end + 1 - bl.offset
		if n > int(needSize) {
			n = int(needSize)
			err = io.EOF
		}
		atomic.AddInt64(&fd.currentSize, int64(n))
		nw, err1 := bl.file.WriteAt(buf[:n], bl.offset)
		if err1 != nil {
			fmt.Println("err1:", err1)
			return err1
		}
		bl.offset += int64(nw)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
	return nil
}

func (bl *BlockFile) startDownload(urlAddress string, fd *FileDownload) (filesize int64, err error) {
	filesize = bl.end - bl.start
	isExit, start := isFileExit(bl.filename, filesize)
	if bl.file != nil {
		defer bl.file.Close()
	}
	if isExit {
		if start < 0 {
			bl.offset = filesize
			atomic.AddInt64(&fd.currentSize, filesize)
			atomic.AddInt64(&fd.preSize, filesize)
			return filesize, nil
		}
		bl.file, err = os.OpenFile(bl.filename, os.O_WRONLY|os.O_RDONLY, 0777)
		if err != nil {
			return
		}
	} else {
		bl.file, err = os.Create(bl.filename)
		if err != nil {
			return filesize, err
		}
	}

	bl.start += start
	bl.offset = start
	atomic.AddInt64(&fd.currentSize, start)
	atomic.AddInt64(&fd.preSize, start)
	err = bl.download(urlAddress, fd)
	if err != nil {
		return
	}
	return // 下载完成
}

// 文件是否存在
func isFileExit(filename string, filesize int64) (r bool, start int64) {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false, 0
	}

	if info.Size() >= filesize {
		return true, -1
	}

	return true, info.Size()
}

func httpGetDir(address string, filepath string) {
	req, err := http.Get(address)
	if err != nil {
		log.Fatal(err)
	}

	doc, err := goquery.NewDocumentFromResponse(req)
	if err != nil {
		return
	}

	doc.Find("a").Each(func(i int, s *goquery.Selection) {
		str := s.Text()
		if string(str[len(str)-1]) == "/" {
			httpGet(address+str, filepath+str)
		} else {
			f := func() error {
				fd, err := NewFileDownload(address+str, filepath)
				if err != nil {
					return err
				}
				return fd.download()
			}
			downloadJob(f)
		}
	})
}

func httpGet(address string, filepath string) (err error) {

	fd, err := NewFileDownload(address, filepath)
	if err != nil {
		return err
	}
	return fd.download()
}

func downloadJob(f func() error) {
	ch <- 1
	wg.Add(1)
	go func() {
		defer func() {
			<-ch
			wg.Done()
			if err := recover(); err != nil {
				fmt.Println("出现错误:", err)
			}
		}()
		err := f()
		if err != nil {
			fmt.Println("出现错误:", err)
		}
	}()
}

func byteFormatPrint(val int64) (s string) {
	return byteFormatPrintFloat(float64(val))
}

func byteFormatPrintFloat(val float64) (s string) {
	strs := []string{"B", "KB", "M", "G", "T"}
	index := 0
	for index < len(strs) {
		if val < 1024 {
			return fmt.Sprintf("%.3f%s", val, strs[index])
		}
		val /= float64(1024)
		index++
	}
	return fmt.Sprintf("%.3f%s", val, strs[index])
}

type params struct {
	*flag.FlagSet `json:"-"`
	workNum       int64
	dir           string
	dirFlag       bool
	url           string
	help          bool
}

func main() {
	p := &params{}
	p.FlagSet = flag.NewFlagSet("go-download Params", flag.ContinueOnError)
	p.StringVar(&p.dir, "dir", "download/", "保存文件夹路径")
	p.StringVar(&p.url, "url", "", "url 地址")
	p.Int64Var(&p.workNum, "work-num", 100, "下载工作池数量")
	p.BoolVar(&p.help, "h", false, "help")
	p.BoolVar(&p.dirFlag, "dir-flag", false, "是否文件夹下载")
	err := p.Parse(os.Args[1:])
	if err != nil {
		os.Exit(0)
	}

	if p.help {
		p.Usage()
		return
	}
	if p.url == "" {
		p.Usage()
		return
	}

	if string(p.dir[len(p.dir)-1]) != "/" {
		p.dir += "/"
	}
	fmt.Println("url:", p.url)
	fmt.Println("work-num:", p.workNum)
	fmt.Println("dir:", p.dir)
	fmt.Println("dir-flag:", p.dirFlag)
	if p.dirFlag {
		ch = make(chan int, 100)
		wg = new(sync.WaitGroup)
		httpGetDir(p.url, p.dir)
		wg.Wait()

	} else {
		err := httpGet(p.url, p.dir)
		if err != nil {
			fmt.Println("下载失败:", err)
			return
		}
	}

	fmt.Println("download success...")
	fmt.Println("下载完成了")
}
