package core

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	gopath "path"
	"strings"
	"sync"
	"time"

	"github.com/aler9/gortsplib"
	"github.com/aler9/gortsplib/pkg/rtpaac"
	"github.com/aler9/gortsplib/pkg/rtph264"
	"github.com/asticode/go-astits"
	"github.com/grafov/m3u8"

	"github.com/aler9/rtsp-simple-server/internal/aac"
	"github.com/aler9/rtsp-simple-server/internal/h264"
	"github.com/aler9/rtsp-simple-server/internal/logger"
	"github.com/aler9/rtsp-simple-server/internal/rtcpsenderset"
)

const (
	hlsSourceRetryPause       = 5 * time.Second
	hlsSourceMinDownloadPause = 1 * time.Second
)

func hlsSourceURLAbsolute(base *url.URL, relative string) (*url.URL, error) {
	u, err := url.Parse(relative)
	if err != nil {
		return nil, err
	}

	if !u.IsAbs() {
		u = &url.URL{
			Scheme: base.Scheme,
			User:   base.User,
			Host:   base.Host,
			Path:   gopath.Join(gopath.Dir(base.Path), relative),
		}
	}

	return u, nil
}

type hlsSourceInstance struct {
	s *hlsSource

	ctx       context.Context
	ctxCancel func()
	ur        *url.URL

	lastDownloadTime      time.Time
	downloadedSegmentURIs []string
	segmentQueueMutex     sync.Mutex
	segmentQueue          [][]byte
	segmentQueueFilled    chan struct{}

	pmtDownloaded bool

	videoPID         *uint16
	videoSPS         []byte
	videoPPS         []byte
	videoTrack       *gortsplib.Track
	videoTrackID     int
	videoEncoder     *rtph264.Encoder

	audioPID         *uint16
	audioConf        *gortsplib.TrackConfigAAC
	audioTrack       *gortsplib.Track
	audioTrackID     int
	audioEncoder     *rtpaac.Encoder

	rtcpSenders *rtcpsenderset.RTCPSenderSet
	stream      *stream

	clockInitialized bool
	clockStartDTS    time.Duration
	clockStartRTC    time.Time
}

func newHLSSourceInstance(
	s *hlsSource) *hlsSourceInstance {
	ctx, ctxCancel := context.WithCancel(s.ctx)

	return &hlsSourceInstance{
		s:                  s,
		ctx:                ctx,
		ctxCancel:          ctxCancel,
		segmentQueueFilled: make(chan struct{}, 1),
	}
}

func (si *hlsSourceInstance) close() {
	si.ctxCancel()
}

func (si *hlsSourceInstance) run() error {
	defer func() {
		if si.stream != nil {
			si.s.parent.OnSourceStaticSetNotReady(pathSourceStaticSetNotReadyReq{Source: si.s})
		}
	}()

	innerCtx, innerCtxCancel := context.WithCancel(context.Background())

	downloaderErr := make(chan error)
	go func() {
		downloaderErr <- si.runDownloader(innerCtx)
	}()

	processorErr := make(chan error)
	go func() {
		processorErr <- si.runProcessor(innerCtx)
	}()

	select {
	case err := <-downloaderErr:
		innerCtxCancel()
		<-processorErr
		return err

	case err := <-processorErr:
		innerCtxCancel()
		<-downloaderErr
		return err

	case <-si.ctx.Done():
		innerCtxCancel()
		<-processorErr
		<-downloaderErr
		return fmt.Errorf("terminated")
	}
}

func (si *hlsSourceInstance) runDownloader(innerCtx context.Context) error {
	for {
		_, err := si.fillSegmentQueue(innerCtx)
		if err != nil {
			return err
		}

		// TODO: fermarsi fino a quando i segmenti in coda non diventano <= 2
	}
}

func (si *hlsSourceInstance) runProcessor(innerCtx context.Context) error {
	for {
		si.segmentQueueMutex.Lock()

		if len(si.segmentQueue) == 0 {
			si.segmentQueueMutex.Unlock()

			select {
			case <-si.segmentQueueFilled:
			case <-innerCtx.Done():
				return fmt.Errorf("terminated")
			}

			si.segmentQueueMutex.Lock()
		}

		var seg []byte
		seg, si.segmentQueue = si.segmentQueue[0], si.segmentQueue[1:]

		si.segmentQueueMutex.Unlock()

		err := si.processSegment(seg)
		if err != nil {
			return err
		}
	}
}

func (si *hlsSourceInstance) fillSegmentQueue(innerCtx context.Context) (bool, error) {
	minTime := si.lastDownloadTime.Add(hlsSourceMinDownloadPause)
	now := time.Now()
	if now.Before(minTime) {
		select {
		case <-time.After(minTime.Sub(now)):
		case <-si.ctx.Done():
			return false, fmt.Errorf("terminated")
		}
	}

	si.lastDownloadTime = now

	pl, err := func() (*m3u8.MediaPlaylist, error) {
		if si.ur == nil {
			return si.downloadPrimaryPlaylist(innerCtx)
		}
		return si.downloadStreamPlaylist(innerCtx)
	}()
	if err != nil {
		return false, err
	}

	added := false

	for _, seg := range pl.Segments {
		if seg == nil {
			break
		}

		if !si.segmentWasDownloaded(seg.URI) {
			si.downloadedSegmentURIs = append(si.downloadedSegmentURIs, seg.URI)
			byts, err := si.downloadSegment(innerCtx, seg.URI)
			if err != nil {
				return false, err
			}

			si.segmentQueueMutex.Lock()

			queueWasEmpty := len(si.segmentQueue) == 0
			si.segmentQueue = append(si.segmentQueue, byts)
			added = true

			if queueWasEmpty {
				si.segmentQueueFilled <- struct{}{}
			}

			si.segmentQueueMutex.Unlock()
		}
	}

	return added, nil
}

func (si *hlsSourceInstance) segmentWasDownloaded(ur string) bool {
	for _, q := range si.downloadedSegmentURIs {
		if q == ur {
			return true
		}
	}
	return false
}

func (si *hlsSourceInstance) downloadPrimaryPlaylist(innerCtx context.Context) (*m3u8.MediaPlaylist, error) {
	var err error
	si.ur, err = url.Parse(si.s.ur)
	if err != nil {
		return nil, err
	}

	pl, err := si.downloadPlaylist(innerCtx)
	if err != nil {
		return nil, err
	}

	switch plt := pl.(type) {
	case *m3u8.MediaPlaylist:
		return plt, nil

	case *m3u8.MasterPlaylist:
		// take the variant with the highest bandwidth
		var chosenVariant *m3u8.Variant
		for _, v := range plt.Variants {
			if chosenVariant == nil ||
				v.VariantParams.Bandwidth > chosenVariant.VariantParams.Bandwidth {
				chosenVariant = v
			}
		}

		if chosenVariant == nil {
			return nil, fmt.Errorf("no variants found")
		}

		u, err := hlsSourceURLAbsolute(si.ur, chosenVariant.URI)
		if err != nil {
			return nil, err
		}

		si.ur = u

		return si.downloadStreamPlaylist(innerCtx)

	default:
		return nil, fmt.Errorf("invalid playlist")
	}
}

func (si *hlsSourceInstance) downloadStreamPlaylist(innerCtx context.Context) (*m3u8.MediaPlaylist, error) {
	pl, err := si.downloadPlaylist(innerCtx)
	if err != nil {
		return nil, err
	}

	plt, ok := pl.(*m3u8.MediaPlaylist)
	if !ok {
		return nil, fmt.Errorf("invalid playlist")
	}

	return plt, nil
}

func (si *hlsSourceInstance) downloadPlaylist(innerCtx context.Context) (m3u8.Playlist, error) {
	si.s.log(logger.Debug, "downloading playlist %s", si.ur)
	req, err := http.NewRequestWithContext(innerCtx, http.MethodGet, si.ur.String(), nil)
	if err != nil {
		return nil, err
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status code: %d", res.StatusCode)
	}

	pl, _, err := m3u8.DecodeFrom(res.Body, true)
	if err != nil {
		return nil, err
	}

	return pl, nil
}

func (si *hlsSourceInstance) downloadSegment(innerCtx context.Context, segmentURI string) ([]byte, error) {
	u, err := hlsSourceURLAbsolute(si.ur, segmentURI)
	if err != nil {
		return nil, err
	}

	si.s.log(logger.Debug, "downloading segment %s", u)
	req, err := http.NewRequestWithContext(innerCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status code: %d", res.StatusCode)
	}

	byts, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	return byts, nil
}

func (si *hlsSourceInstance) processSegment(byts []byte) error {
	dem := astits.NewDemuxer(context.Background(), bytes.NewReader(byts))

	// parse PMT
	if !si.pmtDownloaded {
		for {
			data, err := dem.NextData()
			if err != nil {
				if err == astits.ErrNoMorePackets {
					return nil
				}
				return err
			}

			if data.PMT != nil {
				si.pmtDownloaded = true

				for _, e := range data.PMT.ElementaryStreams {
					switch e.StreamType {
					case astits.StreamTypeH264Video:
						if si.videoPID != nil {
							return fmt.Errorf("multiple video/audio tracks are not supported")
						}

						v := e.ElementaryPID
						si.videoPID = &v

					case astits.StreamTypeAACAudio:
						if si.audioPID != nil {
							return fmt.Errorf("multiple video/audio tracks are not supported")
						}

						v := e.ElementaryPID
						si.audioPID = &v
					}
				}
				break
			}
		}

		if si.videoPID == nil && si.audioPID == nil {
			return fmt.Errorf("stream doesn't contain tracks with supported codecs (H264 or AAC)")
		}
	}

	for {
		data, err := dem.NextData()
		if err != nil {
			if err == astits.ErrNoMorePackets {
				return nil
			}
			if strings.HasPrefix(err.Error(), "astits: parsing PES data failed") {
				fmt.Println("HERE", err.Error())
				continue
			}
			return err
		}

		if data.PES == nil {
			continue
		}

		if data.PES.Header.OptionalHeader == nil ||
			data.PES.Header.OptionalHeader.PTSDTSIndicator == astits.PTSDTSIndicatorNoPTSOrDTS ||
			data.PES.Header.OptionalHeader.PTSDTSIndicator == astits.PTSDTSIndicatorIsForbidden {
			return fmt.Errorf("PTS is missing")
		}

		pts := time.Duration(float64(data.PES.Header.OptionalHeader.PTS.Base) * float64(time.Second) / 90000)

		now := time.Now()

		if si.videoPID != nil && data.PID == *si.videoPID {
			nalus, err := h264.DecodeAnnexB(data.PES.Data)
			if err != nil {
				return err
			}

			var dts time.Duration
			if data.PES.Header.OptionalHeader.PTSDTSIndicator == astits.PTSDTSIndicatorBothPresent {
				dts = time.Duration(float64(data.PES.Header.OptionalHeader.DTS.Base) * float64(time.Second) / 90000)
			} else {
				dts = pts
			}

			if !si.clockInitialized {
				si.clockInitialized = true
				si.clockStartDTS = pts
				si.clockStartRTC = now
			}

			pts -= si.clockStartDTS
			dts -= si.clockStartDTS

			elapsed := now.Sub(si.clockStartRTC)
			if dts > elapsed {
				fmt.Println("VID SLEEP")
				select {
				case <-si.ctx.Done():
					return fmt.Errorf("terminated")
				case <-time.After(dts - elapsed):
				}
			}

			var outNALUs [][]byte

			for _, nalu := range nalus {
				typ := h264.NALUType(nalu[0] & 0x1F)

				switch typ {
				case h264.NALUTypeSPS:
					if si.videoSPS == nil {
						si.videoSPS = append([]byte(nil), nalu...)

						if si.stream == nil {
							err := si.tryInitializeTracks()
							if err != nil {
								return err
							}
						}
					}

					// remove since it's not needed
					continue

				case h264.NALUTypePPS:
					if si.videoPPS == nil {
						si.videoPPS = append([]byte(nil), nalu...)

						if si.stream == nil {
							err := si.tryInitializeTracks()
							if err != nil {
								return err
							}
						}
					}

					// remove since it's not needed
					continue

				case h264.NALUTypeAccessUnitDelimiter:
					// remove since it's not needed
					continue
				}

				outNALUs = append(outNALUs, nalu)
			}

			if len(outNALUs) == 0 {
				continue
			}

			if si.videoEncoder == nil {
				continue
			}

			pkts, err := si.videoEncoder.Encode(outNALUs, pts)
			if err != nil {
				return fmt.Errorf("error while encoding H264: %v", err)
			}

			for _, pkt := range pkts {
				si.onFrame(si.videoTrackID, pkt)
			}

		} else if si.audioPID != nil && data.PID == *si.audioPID {
			adtsPkts, err := aac.DecodeADTS(data.PES.Data)
			if err != nil {
				return err
			}

			if !si.clockInitialized {
				si.clockInitialized = true
				si.clockStartDTS = pts
				si.clockStartRTC = now
			}

			pts -= si.clockStartDTS

			var aus [][]byte

			pktPts := pts

			fmt.Println("LE", len(adtsPkts))

			for _, pkt := range adtsPkts {
				if si.audioConf == nil {
					si.audioConf = &gortsplib.TrackConfigAAC{
						Type:         pkt.Type,
						SampleRate:   pkt.SampleRate,
						ChannelCount: pkt.ChannelCount,
					}

					if si.stream == nil {
						err := si.tryInitializeTracks()
						if err != nil {
							return err
						}
					}
				}

				elapsed := now.Sub(si.clockStartRTC)
				fmt.Println("DIFF", pktPts - elapsed)
				if pktPts > elapsed {
					select {
					case <-si.ctx.Done():
						return fmt.Errorf("terminated")
					case <-time.After(pktPts - elapsed):
					}
				}

				aus = append(aus, pkt.AU)
				pktPts += time.Duration(1000 * time.Second / time.Duration(pkt.SampleRate))
			}

			if si.audioEncoder == nil {
				continue
			}

			pkts, err := si.audioEncoder.Encode(aus, pts)
			if err != nil {
				return fmt.Errorf("error while encoding AAC: %v", err)
			}

			for _, pkt := range pkts {
				si.onFrame(si.audioTrackID, pkt)
			}
		}
	}
}

func (si *hlsSourceInstance) tryInitializeTracks() error {
	if si.videoPID != nil {
		if si.videoSPS == nil || si.videoPPS == nil {
			return nil
		}
	}

	if si.audioPID != nil {
		if si.audioConf == nil {
			return nil
		}
	}

	var tracks gortsplib.Tracks

	if si.videoPID != nil {
		var err error
		si.videoTrack, err = gortsplib.NewTrackH264(96, &gortsplib.TrackConfigH264{si.videoSPS, si.videoPPS})
		if err != nil {
			return err
		}

		si.videoTrackID = len(tracks)
		tracks = append(tracks, si.videoTrack)
		si.videoEncoder = rtph264.NewEncoder(96, nil, nil, nil)
	}

	if si.audioPID != nil {
		var err error
		si.audioTrack, err = gortsplib.NewTrackAAC(97, si.audioConf)
		if err != nil {
			return err
		}

		si.audioTrackID = len(tracks)
		tracks = append(tracks, si.audioTrack)
		si.audioEncoder = rtpaac.NewEncoder(97, si.audioConf.SampleRate, nil, nil, nil)
	}

	res := si.s.parent.OnSourceStaticSetReady(pathSourceStaticSetReadyReq{
		Source: si.s,
		Tracks: tracks,
	})
	if res.Err != nil {
		return res.Err
	}

	si.s.log(logger.Info, "ready")

	si.stream = res.Stream
	si.rtcpSenders = rtcpsenderset.New(tracks, res.Stream.onFrame)

	return nil
}

func (si *hlsSourceInstance) onFrame(trackID int, payload []byte) {
	si.rtcpSenders.OnFrame(trackID, gortsplib.StreamTypeRTP, payload)
	si.stream.onFrame(trackID, gortsplib.StreamTypeRTP, payload)
}

type hlsSourceParent interface {
	Log(logger.Level, string, ...interface{})
	OnSourceStaticSetReady(req pathSourceStaticSetReadyReq) pathSourceStaticSetReadyRes
	OnSourceStaticSetNotReady(req pathSourceStaticSetNotReadyReq)
}

type hlsSource struct {
	ur     string
	wg     *sync.WaitGroup
	parent hlsSourceParent

	ctx       context.Context
	ctxCancel func()
}

func newHLSSource(
	parentCtx context.Context,
	ur string,
	wg *sync.WaitGroup,
	parent hlsSourceParent) *hlsSource {
	ctx, ctxCancel := context.WithCancel(parentCtx)

	s := &hlsSource{
		ur:        ur,
		wg:        wg,
		parent:    parent,
		ctx:       ctx,
		ctxCancel: ctxCancel,
	}

	s.log(logger.Info, "started")

	s.wg.Add(1)
	go s.run()

	return s
}

func (s *hlsSource) Close() {
	s.log(logger.Info, "stopped")
	s.ctxCancel()
}

func (s *hlsSource) log(level logger.Level, format string, args ...interface{}) {
	s.parent.Log(level, "[hls source] "+format, args...)
}

func (s *hlsSource) run() {
	defer s.wg.Done()

outer:
	for {
		ok := s.runInner()
		if !ok {
			break outer
		}

		select {
		case <-time.After(hlsSourceRetryPause):
		case <-s.ctx.Done():
			break outer
		}
	}

	s.ctxCancel()
}

func (s *hlsSource) runInner() bool {
	si := newHLSSourceInstance(s)

	done := make(chan error)
	go func() {
		done <- si.run()
	}()

	select {
	case err := <-done:
		s.log(logger.Info, "ERR: %v", err)
		return true

	case <-s.ctx.Done():
		si.close()
		<-done
		return false
	}
}

// OnSourceAPIDescribe implements source.
func (*hlsSource) OnSourceAPIDescribe() interface{} {
	return struct {
		Type string `json:"type"`
	}{"hlsSource"}
}
