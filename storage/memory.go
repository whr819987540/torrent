package storage

import (
	"fmt"
	"io"
	"log"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/anacrolix/missinggo/v2"
	"github.com/anacrolix/torrent/common"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/segments"
)

// 专门用来对内存进行读写
// 基于指定的[]byte生成一个buffer, 可以从某个偏移量开始写入指定的数据
// 返回实际写入的字节数, error
type memoryBuf struct {
	data       []byte
	length     int64
	totalWrite int64 // 实际写入的字节数, 这里需要加锁
	mu         sync.Mutex
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func (fb *memoryBuf) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("WriteAt, off < 0")
	}
	n := min(int64(fb.length)-off, int64(len(p))) // n是写入的字节数
	// p的[0,n)写入fb.data的[off,off+n)
	srcSlice := p[0:n]
	dstSlice := fb.data[off : off+n]
	copy(dstSlice, srcSlice)
	fb.mu.Lock()
	fb.totalWrite += n
	fb.mu.Unlock()
	return int(n), nil
}

func (fb *memoryBuf) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("ReadAt, off < 0")
	}
	// 从off开始, 从fb.data中读字节, 直到fb.data结束或p满
	n := min(int64(fb.length)-off, int64(len(p))) // n是读出的字节数
	// fb.data的[off,off+n)写入到p的[0,n)
	srcSlice := fb.data[off : off+n]
	dstSlice := p[0:n]
	copy(dstSlice, srcSlice)

	return int(n), nil
}

// type memoryBuf struct {
// 	*bytes.Buffer
// }

// func (buf *memoryBuf) WriteAt(p []byte, off int64) (n int, err error) {
// 	return buf.WriteAt(p, off)
// }
// func (buf *memoryBuf) ReadAt(p []byte, off int64) (n int, err error) {
// 	return buf.ReadAt(p, off)
// }

// NewMemoryClientOpts 必须和NewFileClientOpts一样, 实现Close/OpenTorrent两个函数
type NewMemoryClientOpts struct {
	Torrent         *memoryBuf
	PieceCompletion PieceCompletion // 用来记录piece completion情况
}

// MemoryClientImpl is Memory-based storage for torrents, that isn't yet bound to a particular torrent.
type MemoryClientImpl struct {
	opts NewMemoryClientOpts
}

// Close 关闭操作piece completion的sqlite连接
func (mci MemoryClientImpl) Close() error {
	return mci.opts.PieceCompletion.Close()
}

func (mci MemoryClientImpl) BytesWrittenToMemory() int64 {
	return mci.opts.Torrent.totalWrite
}

// OpenTorrent 是ClientImpl接口中定义的一个函数, 必须实现
// 功能就是给定一个
func (mci MemoryClientImpl) OpenTorrent(info *metainfo.Info, infoHash metainfo.Hash) (_ TorrentImpl, err error) {
	// // Data storage bound to a torrent.
	// type TorrentImpl struct {
	// 	Piece func(p metainfo.Piece) PieceImpl
	// 	Close func() error
	// 	Flush func() error
	// 	// Storages that share the same space, will provide equal pointers. The function is called once
	// 	// to determine the storage for torrents sharing the same function pointer, and mutated in
	// 	// place.
	// 	Capacity TorrentCapacity
	// }
	upvertedFiles := info.UpvertedFiles()
	files := make([]file, 0, len(upvertedFiles))
	for _, fileInfo := range upvertedFiles {
		f := file{
			path:   filepath.Join(fileInfo.Path...),
			length: fileInfo.Length,
		}
		files = append(files, f)
	}
	fmt.Print(upvertedFiles)
	t := memoryTorrentImpl{
		Torrent:        mci.opts.Torrent,
		files:          files, // from torrent
		segmentLocater: segments.NewIndex(common.LengthIterFromUpvertedFiles(upvertedFiles)),
		infoHash:       infoHash,
		completion:     mci.opts.PieceCompletion,
	}
	return TorrentImpl{
		Piece: t.Piece,
		Close: t.Close,
	}, nil
}

// NewMemory creates a memory block to store all Torrent data
func NewMemory(totalLength int64) (ClientImplCloser, error) {
	return NewMemoryWithCompletion(totalLength)
}

// NewMemoryWithCompletion 中Completion是指piece completion, 用来记录piece的完成情况(已经实现的是sqlite)
// 但这个并不是必须的
func NewMemoryWithCompletion(totalLength int64) (ClientImplCloser, error) {
	// allocate memory
	if flag, avail := isAllocatedOutOfHeapMemory(totalLength); !flag {
		return nil, fmt.Errorf("try to allocate too much memory, want %d, available %d", totalLength, avail)
	}
	data := make([]byte, totalLength)
	var p *byte = &data[0]
	log.Printf("%d %d %v", len(data), cap(data), p)

	// storage.ClientImplCloser 实际返回 memoryClientImpl
	opts := NewMemoryClientOpts{
		Torrent: &memoryBuf{
			data:   data,
			length: totalLength,
			totalWrite: 0,
		},
		PieceCompletion: pieceCompletionForDir("./"), // 默认选择sqlite, sqlite的db文件放在./下面
	}
	return MemoryClientImpl{opts}, nil
}

// NewMemoryOpts creates a new MemoryImplCloser that stores files using memory
func NewMemoryOpts(opts NewMemoryClientOpts) ClientImplCloser {
	if opts.Torrent == nil {
		return nil
	}
	if opts.PieceCompletion == nil {
		opts.PieceCompletion = pieceCompletionForDir("./")
	}
	return MemoryClientImpl{opts: opts}
}

// 检查待分配的内存是否超过堆的大小
func isAllocatedOutOfHeapMemory(allocated int64) (flag bool, available uint64) {
	var m runtime.MemStats
	if uint64(allocated) > m.HeapIdle {
		flag = true
	}
	available = m.HeapIdle
	return
}

// memoryTorrentImpl是对TorrentImpl接口的一个实现
//
//	type TorrentImpl struct {
//		Piece func(p metainfo.Piece) PieceImpl
//		Close func() error
//		Flush func() error
//		// Storages that share the same space, will provide equal pointers. The function is called once
//		// to determine the storage for torrents sharing the same function pointer, and mutated in
//		// place.
//		Capacity TorrentCapacity
//	}
type memoryTorrentImpl struct {
	Torrent        *memoryBuf
	files          []file
	segmentLocater segments.Index
	infoHash       metainfo.Hash
	completion     PieceCompletion
}

func (mti *memoryTorrentImpl) Piece(p metainfo.Piece) PieceImpl {
	// Create a view onto the file-based torrent storage.
	_io := memoryTorrentImplIO{mti}
	// Return the appropriate segments of this.
	return &memoryPieceImpl{
		mti,
		p,
		missinggo.NewSectionWriter(_io, p.Offset(), p.Length()),
		io.NewSectionReader(_io, p.Offset(), p.Length()),
	}
}

func (fs *memoryTorrentImpl) Close() error {
	return nil
}

// 实际进行IO操作的部分
// 需要实现ReadAt/WriteAt
type memoryTorrentImplIO struct {
	mti *memoryTorrentImpl
}

// Returns EOF on short or missing file.
func (mtii *memoryTorrentImplIO) readFileAt(file file, b []byte, off int64) (n int, err error) {
	// f, err := os.Open(file.path)
	// if os.IsNotExist(err) {
	// 	// File missing is treated the same as a short file.
	// 	err = io.EOF
	// 	return
	// }
	// if err != nil {
	// 	return
	// }
	// defer f.Close()

	f := mtii.mti.Torrent

	// Limit the read to within the expected bounds of this file.
	if int64(len(b)) > file.length-off {
		b = b[:file.length-off]
	}
	for off < file.length && len(b) != 0 {
		n1, err1 := f.ReadAt(b, off)
		b = b[n1:]
		n += n1
		off += int64(n1)
		if n1 == 0 {
			err = err1
			break
		}
	}
	return
}

// Only returns EOF at the end of the torrent. Premature EOF is ErrUnexpectedEOF.
func (mtii memoryTorrentImplIO) ReadAt(b []byte, off int64) (n int, err error) {
	mtii.mti.segmentLocater.Locate(
		segments.Extent{
			Start:  off,
			Length: int64(len(b)),
		},
		func(i int, e segments.Extent) bool {
			n1, err1 := mtii.readFileAt(mtii.mti.files[i], b[:e.Length], e.Start)
			n += n1
			b = b[n1:]
			err = err1
			return err == nil // && int64(n1) == e.Length
		})
	if len(b) != 0 && err == nil {
		err = io.EOF
	}
	return
}

func (mtii memoryTorrentImplIO) WriteAt(p []byte, off int64) (n int, err error) {
	// log.Printf("write at %v: %v bytes", off, len(p))
	mtii.mti.segmentLocater.Locate(
		segments.Extent{
			Start:  off,
			Length: int64(len(p)),
		},
		func(i int, e segments.Extent) bool {
			// name := mtii.mti.files[i].path
			// os.MkdirAll(filepath.Dir(name), 0o777)
			// var f *os.File
			// f, err = os.OpenFile(name, os.O_WRONLY|os.O_CREATE, 0o666)
			// if err != nil {
			// 	return false
			// }

			// 需要从mtii中知道Torrent(写入位置)
			f := mtii.mti.Torrent

			var n1 int
			n1, err = f.WriteAt(p[:e.Length], e.Start)
			// log.Printf("%v %v wrote %v: %v", i, e, n1, err)

			n += n1
			p = p[n1:]
			if int64(n1) != e.Length {
				err = io.ErrShortWrite
			}
			return err == nil
		})
	return
}

// memoryPieceImpl是对PieceImpl接口的一个实现
type memoryPieceImpl struct {
	*memoryTorrentImpl
	p metainfo.Piece
	io.WriterAt
	io.ReaderAt
}

// 获取piece的key
func (mpi *memoryPieceImpl) pieceKey() metainfo.PieceKey {
	return metainfo.PieceKey{mpi.infoHash, mpi.p.Index()}
}

// 获取某个piece的完成情况
func (mpi *memoryPieceImpl) Completion() Completion {
	c, err := mpi.completion.Get(mpi.pieceKey())
	if err != nil {
		log.Printf("error getting piece completion: %s", err)
		c.Ok = false
		return c
	}

	// 对于内存存储, 由于不需要写入磁盘, 无需检查
	// verified := true
	// if c.Complete {
	// 	// If it's allegedly complete, check that its constituent files have the necessary length.
	// 	for _, fi := range extentCompleteRequiredLengths(mpi.p.Info, mpi.p.Offset(), mpi.p.Length()) {
	// 		s, err := os.Stat(mpi.files[fi.fileIndex].path)
	// 		if err != nil || s.Size() < fi.length {
	// 			verified = false
	// 			break
	// 		}
	// 	}
	// }

	// if !verified {
	// 	// The completion was wrong, fix it.
	// 	c.Complete = false
	// 	mpi.completion.Set(mpi.pieceKey(), false)
	// }

	return c
}

// 将某个piece标记为完成
func (mpi *memoryPieceImpl) MarkComplete() error {
	return mpi.completion.Set(mpi.pieceKey(), true)
}

// 将某个piece标记未完成
func (mpi *memoryPieceImpl) MarkNotComplete() error {
	return mpi.completion.Set(mpi.pieceKey(), false)
}
