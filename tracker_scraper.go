package torrent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/anacrolix/dht/v2/krpc"
	"github.com/anacrolix/log"

	"github.com/anacrolix/torrent/tracker"
)

// Announces a torrent to a tracker at regular intervals, when peers are
// required.
type trackerScraper struct {
	u               url.URL
	t               *Torrent
	lastAnnounce    trackerAnnounceResult
	lookupTrackerIp func(*url.URL) ([]net.IP, error)
}

type torrentTrackerAnnouncer interface {
	statusLine() string
	URL() *url.URL
}

func (me trackerScraper) URL() *url.URL {
	return &me.u
}

func (ts *trackerScraper) statusLine() string {
	var w bytes.Buffer
	fmt.Fprintf(&w, "next ann: %v, last ann: %v",
		func() string {
			// 计算下一次 announce 的时间
			na := time.Until(ts.lastAnnounce.Completed.Add(ts.lastAnnounce.Interval))
			if na > 0 {
				na /= time.Second
				na *= time.Second
				return na.String()
			} else {
				// 马上可以进行下一次 announce
				return "anytime"
			}
		}(),
		func() string {
			// 上一次 announce 出错
			if ts.lastAnnounce.Err != nil {
				return ts.lastAnnounce.Err.Error()
			}
			// 上一次 announce 从未成功完成过, 即没有成功完成的 announce
			if ts.lastAnnounce.Completed.IsZero() {
				return "never"
			}
			// 上一次 announce 完成时获取的peer数
			return fmt.Sprintf("%d peers", ts.lastAnnounce.NumPeers)
		}(),
	)
	return w.String()
}

type trackerAnnounceResult struct {
	Err       error
	NumPeers  int
	Interval  time.Duration
	Completed time.Time
}

func (me *trackerScraper) getIp() (ip net.IP, err error) {
	trackerURL := me.u.String()
	var ips []net.IP
	// 内置函数查找tracker IP
	if me.lookupTrackerIp != nil {
		ips, err = me.lookupTrackerIp(&me.u)
	} else {
		// Do a regular dns lookup
		ips, err = net.LookupIP(me.u.Hostname())
	}
	if err != nil {
		return
	}
	// 内置函数或dns 查询结果为空
	if len(ips) == 0 {
		err = fmt.Errorf("no ips(couldn't know the tracker's(%s) ip)", trackerURL)
		return
	}
	me.t.cl.rLock()
	defer me.t.cl.rUnlock()
	if me.t.cl.closed.IsSet() {
		err = errors.New("client is closed")
		return
	}
	if Debug {
		log.Printf("dns resolve: tracker(%s),ips(%v)", trackerURL, ips)
	}

	for _, ip = range ips {
		if me.t.cl.ipIsBlocked(ip) {
			if Debug {
				log.Printf("%s(%s) blocked", me.u.String(), ip.String())
			}
			continue
		}
		switch me.u.Scheme {
		case "udp4":
			if ip.To4() == nil {
				continue
			}
		case "udp6":
			if ip.To4() != nil {
				continue
			}
		default:
			if Debug {
				log.Printf("%s", me.u.Scheme)
			}
		}
		return
	}
	err = errors.New("no acceptable ips")
	return
}

func (me *trackerScraper) trackerUrl(ip net.IP) string {
	u := me.u
	if u.Port() != "" {
		u.Host = net.JoinHostPort(ip.String(), u.Port())
	}
	return u.String()
}

// Return how long to wait before trying again. For most errors, we return 5
// minutes, a relatively quick turn around for DNS changes.
func (me *trackerScraper) announce(ctx context.Context, event tracker.AnnounceEvent) (ret trackerAnnounceResult) {
	defer func() {
		ret.Completed = time.Now()
	}()
	ret.Interval = time.Minute

	// Limit concurrent use of the same tracker URL by the Client.
	ref := me.t.cl.activeAnnounceLimiter.GetRef(me.u.String())
	defer ref.Drop()
	select {
	case <-ctx.Done():
		ret.Err = ctx.Err()
		return
	case ref.C() <- struct{}{}:
	}
	defer func() {
		select {
		case <-ref.C():
		default:
			panic("should return immediately")
		}
	}()

	// tracker ip
	ip, err := me.getIp()
	if err != nil {
		ret.Err = fmt.Errorf("error getting ip: %s", err)
		return
	}
	me.t.cl.rLock()
	req := me.t.announceRequest(event)
	me.t.cl.rUnlock()
	// The default timeout works well as backpressure on concurrent access to the tracker. Since
	// we're passing our own Context now, we will include that timeout ourselves to maintain similar
	// behavior to previously, albeit with this context now being cancelled when the Torrent is
	// closed.
	// 15s
	ctx, cancel := context.WithTimeout(ctx, tracker.DefaultTrackerAnnounceTimeout)
	defer cancel()
	me.t.logger.WithDefaultLevel(log.Debug).Printf("announcing to %q: %#v", me.u.String(), req)
	res, err := tracker.Announce{
		Context:             ctx,
		HttpProxy:           me.t.cl.config.HTTPProxy,
		HttpRequestDirector: me.t.cl.config.HttpRequestDirector,
		DialContext:         me.t.cl.config.TrackerDialContext,
		ListenPacket:        me.t.cl.config.TrackerListenPacket,
		UserAgent:           me.t.cl.config.HTTPUserAgent,
		TrackerUrl:          me.trackerUrl(ip),
		Request:             req,
		HostHeader:          me.u.Host,
		ServerName:          me.u.Hostname(),
		UdpNetwork:          me.u.Scheme,
		ClientIp4:           krpc.NodeAddr{IP: me.t.cl.config.PublicIp4},
		ClientIp6:           krpc.NodeAddr{IP: me.t.cl.config.PublicIp6},
		Logger:              me.t.logger,
	}.Do()
	me.t.logger.WithDefaultLevel(log.Debug).Printf("announce to %q returned %#v: %v", me.u.String(), res, err)
	if err != nil {
		if Debug {
			log.Printf("annouce error:%v", err)
		}
		ret.Err = fmt.Errorf("announcing: %w", err)
		return
	}
	log.Printf("announce to %q(%s) returned %#v, err is %v, interval is %v/s.", me.u.String(), ip, res, err, res.Interval)

	me.t.AddPeers(peerInfos(nil).AppendFromTracker(res.Peers))
	ret.NumPeers = len(res.Peers)
	// set the announce interval
	ret.Interval = time.Duration(res.Interval) * time.Second
	return
}

// Returns whether we can shorten the interval, and sets notify to a channel that receives when we
// might change our mind, or leaves it if we won't.
func (me *trackerScraper) canIgnoreInterval(notify *<-chan struct{}) bool {
	gotInfo := me.t.GotInfo()
	select {
	case <-gotInfo:
		// Private trackers really don't like us announcing more than they specify. They're also
		// tracking us very carefully, so it's best to comply.
		private := me.t.info.Private
		return private == nil || !*private
	default:
		*notify = gotInfo
		return false
	}
}

func (me *trackerScraper) Run() {
	defer me.announceStopped()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer cancel()
		select {
		case <-ctx.Done():
		case <-me.t.Closed():
		}
	}()

	// make sure first announce is a "started"
	e := tracker.Started

	for {
		ar := me.announce(ctx, e)
		// after first announce, get back to regular "none"
		e = tracker.None
		me.t.cl.lock()
		me.lastAnnounce = ar
		me.t.cl.unlock()

	recalculate:
		// Make sure we don't announce for at least a minute since the last one.
		interval := ar.Interval
		// TODO: make peer find each other as soon as possible
		// DONE: remove restrictions on announce interval
		// if interval < time.Minute {
		// 	interval = time.Minute
		// }

		me.t.cl.lock()
		wantPeers := me.t.wantPeersEvent.C()
		me.t.cl.unlock()

		// If we want peers, reduce the interval to the minimum if it's appropriate.

		// A channel that receives when we should reconsider our interval. Starts as nil since that
		// never receives.
		var reconsider <-chan struct{}
		select {
		case <-wantPeers:
			if interval > time.Minute && me.canIgnoreInterval(&reconsider) {
				interval = time.Minute
			}
		default:
			reconsider = wantPeers
		}

		select {
		case <-me.t.closed.Done():
			return
		case <-reconsider:
			// Recalculate the interval.
			goto recalculate
		case <-time.After(time.Until(ar.Completed.Add(interval))):
		}
	}
}

func (me *trackerScraper) announceStopped() {
	ctx, cancel := context.WithTimeout(context.Background(), tracker.DefaultTrackerAnnounceTimeout)
	defer cancel()
	me.announce(ctx, tracker.Stopped)
}
