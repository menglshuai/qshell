package qshell

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/qiniu/api/auth/digest"
	fio "github.com/qiniu/api/io"
	rio "github.com/qiniu/api/resumable/io"
	"github.com/qiniu/api/rs"
	"github.com/qiniu/log"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"io/ioutil"
	"os"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

/*
Config file like:

{
	"src_dir" 		:	"/Users/jemy/Photos",
	"access_key" 	:	"<Your AccessKey>",
	"secret_key"	:	"<Your SecretKey>",
	"bucket"		:	"test-bucket",
	"ignore_dir"	:	false,
	"key_prefix"	:	"2014/12/01/",
	"overwrite"		:	false
}

or without key_prefix and ignore_dir

{
	"src_dir" 		:	"/Users/jemy/Photos",
	"access_key" 	:	"<Your AccessKey>",
	"secret_key"	:	"<Your SecretKey>",
	"bucket"		:	"test-bucket",
}
*/

const (
	PUT_THRESHOLD           int64 = 100 * 1 << 20
	MIN_UPLOAD_THREAD_COUNT int64 = 1
	MAX_UPLOAD_THREAD_COUNT int64 = 100
)

type UploadConfig struct {
	SrcDir    string `json:"src_dir"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	Bucket    string `json:"bucket"`
	KeyPrefix string `json:"key_prefix,omitempty"`
	IgnoreDir bool   `json:"ignore_dir,omitempty"`
	Overwrite bool   `json:"overwrite,omitempty"`
}

func QiniuUpload(threadCount int, uploadConfigFile string) {
	fp, err := os.Open(uploadConfigFile)
	if err != nil {
		log.Error(fmt.Sprintf("Open upload config file `%s' error due to `%s'", uploadConfigFile, err))
		return
	}
	defer fp.Close()
	configData, err := ioutil.ReadAll(fp)
	if err != nil {
		log.Error(fmt.Sprintf("Read upload config file `%s' error due to `%s'", uploadConfigFile, err))
		return
	}
	var uploadConfig UploadConfig
	err = json.Unmarshal(configData, &uploadConfig)
	if err != nil {
		log.Error(fmt.Sprintf("Parse upload config file `%s' errror due to `%s'", uploadConfigFile, err))
		return
	}
	if _, err := os.Stat(uploadConfig.SrcDir); err != nil {
		log.Error("Upload config error for parameter `SrcDir`,", err)
		return
	}
	dirCache := DirCache{}
	currentUser, err := user.Current()
	if err != nil {
		log.Error("Failed to get current user", err)
		return
	}
	pathSep:=string(os.PathSeparator)
	jobId := base64.URLEncoding.EncodeToString([]byte(uploadConfig.SrcDir + ":" + uploadConfig.Bucket))
	storePath := fmt.Sprintf("%s%s.qshell%squpload%s%s", currentUser.HomeDir,pathSep,pathSep, pathSep,jobId)
	err = os.MkdirAll(storePath, 0775)
	if err != nil {
		log.Error(fmt.Sprintf("Failed to mkdir `%s' due to `%s'", storePath, err))
		return
	}
	cacheFileName := fmt.Sprintf("%s%s%s.cache", storePath,pathSep, jobId)
	leveldbFileName := fmt.Sprintf("%s%s%s.ldb", storePath,pathSep, jobId)
	totalFileCount := dirCache.Cache(uploadConfig.SrcDir, cacheFileName)
	ldb, err := leveldb.OpenFile(leveldbFileName, nil)
	if err != nil {
		log.Error(fmt.Sprintf("Open leveldb `%s' failed due to `%s'", leveldbFileName, err))
		return
	}
	defer ldb.Close()
	//sync
	ufp, err := os.Open(cacheFileName)
	if err != nil {
		log.Error(fmt.Sprintf("Open cache file `%s' failed due to `%s'", cacheFileName, err))
		return
	}
	defer ufp.Close()
	bScanner := bufio.NewScanner(ufp)
	bScanner.Split(bufio.ScanLines)
	currentFileCount := 0
	ldbWOpt := opt.WriteOptions{
		Sync: true,
	}

	upWorkGroup := sync.WaitGroup{}
	upCounter := 0
	threadThreshold := threadCount + 1

	mac := digest.Mac{uploadConfig.AccessKey, []byte(uploadConfig.SecretKey)}
	//check thread count
	for bScanner.Scan() {
		line := strings.TrimSpace(bScanner.Text())
		items := strings.Split(line, "\t")
		if len(items) > 1 {
			cacheFname := items[0]
			cacheFlmd, _ := strconv.Atoi(items[2])
			uploadFileKey := cacheFname
			if uploadConfig.IgnoreDir {
				if i := strings.LastIndex(uploadFileKey, pathSep); i != -1 {
					uploadFileKey = uploadFileKey[i+1:]
				}
			}
			if uploadConfig.KeyPrefix != "" {
				uploadFileKey = strings.Join([]string{uploadConfig.KeyPrefix, uploadFileKey}, "")
			}
			//convert \ to / under windows
			if runtime.GOOS == "windows" {
				uploadFileKey = strings.Replace(uploadFileKey, "\\", "/", -1)
			}
			cacheFilePath := strings.Join([]string{uploadConfig.SrcDir, cacheFname}, pathSep)
			fstat, err := os.Stat(cacheFilePath)
			if err != nil {
				log.Error(fmt.Sprintf("Error stat local file `%s' due to `%s'", cacheFilePath, err))
				return
			}
			fsize := fstat.Size()

			//check leveldb
			currentFileCount += 1
			ldbKey := fmt.Sprintf("%s => %s", cacheFilePath, uploadFileKey)
			log.Debug(fmt.Sprintf("Checking %s ...", ldbKey))
			//check last modified
			ldbFlmd, err := ldb.Get([]byte(ldbKey), nil)
			flmd, _ := strconv.Atoi(string(ldbFlmd))
			//not exist, return ErrNotFound
			if err == nil && cacheFlmd == flmd {
				continue
			}

			fmt.Print("\033[2K\r")
			fmt.Printf("Uploading %s (%d/%d, %.0f%%) ...", ldbKey, currentFileCount, totalFileCount,
				float32(currentFileCount)*100/float32(totalFileCount))
			os.Stdout.Sync()
			//worker
			upCounter += 1
			if upCounter%threadThreshold == 0 {
				upWorkGroup.Wait()
			}
			upWorkGroup.Add(1)
			go func() {
				defer upWorkGroup.Done()

				policy := rs.PutPolicy{}
				policy.Scope = uploadConfig.Bucket
				if uploadConfig.Overwrite {
					policy.Scope = uploadConfig.Bucket + ":" + uploadFileKey
					policy.InsertOnly = 0
				}
				policy.Expires = 24 * 3600
				uptoken := policy.Token(&mac)
				if fsize > PUT_THRESHOLD {
					putRet := rio.PutRet{}
					err := rio.PutFile(nil, &putRet, uptoken, uploadFileKey, cacheFilePath, nil)
					if err != nil {
						log.Error(fmt.Sprintf("Put file `%s' => `%s' failed due to `%s'", cacheFilePath, uploadFileKey, err))
					} else {
						perr := ldb.Put([]byte(ldbKey), []byte("Y"), &ldbWOpt)
						if perr != nil {
							log.Error(fmt.Sprintf("Put key `%s' into leveldb error due to `%s'", ldbKey, perr))
						}
					}
				} else {
					putRet := fio.PutRet{}
					err := fio.PutFile(nil, &putRet, uptoken, uploadFileKey, cacheFilePath, nil)
					if err != nil {
						log.Error(fmt.Sprintf("Put file `%s' => `%s' failed due to `%s'", cacheFilePath, uploadFileKey, err))
					} else {
						perr := ldb.Put([]byte(ldbKey), []byte(strconv.Itoa(cacheFlmd)), &ldbWOpt)
						if perr != nil {
							log.Error(fmt.Sprintf("Put key `%s' into leveldb error due to `%s'", ldbKey, perr))
						}
					}
				}
			}()
		} else {
			log.Error(fmt.Sprintf("Error cache line `%s'", line))
		}
	}
	upWorkGroup.Wait()
	fmt.Println()
	fmt.Println("Upload done!")
}
