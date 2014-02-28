package memcache

import (
    "log"
    "os"
    "sync"
    "sync/atomic"
    "unsafe"
    "runtime"
)
var AccessLogPath string
var ErrorLogPath string
var AccessLog *log.Logger = nil
var ErrorLog *log.Logger = nil
var AccessFd *os.File = nil
var ErrorFd *os.File = nil
var lock *sync.Mutex = new(sync.Mutex)

func openLogWithFd(fd *os.File) *log.Logger {
    return log.New(fd, "", log.Ldate|log.Ltime|log.Lmicroseconds)
}

func openLog(path string) (logger *log.Logger, fd *os.File, err error) {
    if fd, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644); err == nil {
        logger = openLogWithFd(fd)
    }
    return
}

func OpenAccessLog(access_log_path string) (success bool, err error) {
    lock.Lock()
    defer lock.Unlock()
    success = false
    if AccessLog == nil {
        if AccessLog, AccessFd, err = openLog(access_log_path); err == nil {
            success = true
        }
    } else {
        // start swap exist logger and new logger, and Close the older fd in later, if it is Stdout, leave it
        access_log, access_file, e := openLog(access_log_path)
        err = e
        if err == nil {
            success = true
            access_log = (*log.Logger)(atomic.SwapPointer((*unsafe.Pointer)(unsafe.Pointer(&AccessLog)), unsafe.Pointer(access_log)))
            access_file = (*os.File)(atomic.SwapPointer((*unsafe.Pointer)(unsafe.Pointer(&AccessFd)), unsafe.Pointer(access_file)))
            runtime.SetFinalizer(access_file, func(f *os.File) {
                if e := f.Close(); e == nil {
                    log.Println("close the old accesslog fd success!")
                } else {
                    log.Println("close the old accesslog fd failure with, ", e)
                }
            })
        }
    }
    if !success {
        log.Println("open " + access_log_path + " failed: " + err.Error())
    }
    return
}

func OpenErrorLog(error_log_path string) (success bool, err error) {
    lock.Lock()
    defer lock.Unlock()
    success = false
    if ErrorLog == nil {
        if ErrorLog, ErrorFd, err = openLog(error_log_path); err == nil {
            success = true
        }
    } else {
        // start swap exist logger and new logger, and Close the older fd in later, if it is Stdout, leave it
        error_log, error_file, e := openLog(error_log_path)
        err = e
        if err == nil {
            success = true
            error_log = (*log.Logger)(atomic.SwapPointer((*unsafe.Pointer)(unsafe.Pointer(&ErrorLog)), unsafe.Pointer(error_log)))
            error_file = (*os.File)(atomic.SwapPointer((*unsafe.Pointer)(unsafe.Pointer(&ErrorFd)), unsafe.Pointer(error_file)))
            runtime.SetFinalizer(error_file, func(f *os.File) {
                if e := f.Close(); e == nil {
                    log.Println("close the old errorlog fd success!")
                } else {
                    log.Println("close the old errorlog fd failure with, ", e)
                }
            })
        }
    }
    if !success {
        log.Println("open " + error_log_path + " failed: " + err.Error())
    }
    return
}