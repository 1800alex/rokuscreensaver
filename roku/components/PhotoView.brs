sub init()
    m.photo = m.top.findNode("photo")
    m.timer = m.top.findNode("refresh")
    m.retry = m.top.findNode("retry")
    m.burnin = m.top.findNode("burnin")
    m.errorBg = m.top.findNode("errorBg")
    m.errorMsg = m.top.findNode("errorMsg")
    m.lastId = ""
    m.hasFrame = false   ' have we ever shown a real frame?
    m.failStart = 0      ' unix seconds of the first failure in the current outage (0 = ok)

    ' Positions the (1400x300) message block drifts between. Bounds: x 0..520,
    ' y 0..780 so it always stays fully on a 1920x1080 screen.
    m.burnSpots = [[40, 60], [500, 60], [40, 720], [500, 720], [270, 390]]
    m.burnIdx = 0

    ' One persistent fetch worker, reused for the whole session (see ImageFetcher).
    m.fetcher = CreateObject("roSGNode", "ImageFetcher")
    m.fetcher.observeField("result", "onFetched")
    m.fetcher.control = "RUN"

    m.photo.observeField("loadStatus", "onLoadStatus")
    m.timer.observeField("fire", "onTick")
    m.retry.observeField("fire", "onRetry")
    m.burnin.observeField("fire", "onBurnInTick")
    m.top.observeField("running", "onRunningChange")
end sub

sub onRunningChange()
    if m.top.running
        startCycling()
    else
        stopCycling()
    end if
end sub

sub startCycling()
    m.errorMsg.text = "Can't reach the photo server" + Chr(10) + Chr(10) + m.top.feedUrl + Chr(10) + Chr(10) + "Retrying every " + m.top.intervalSeconds.ToStr() + "s..."

    iv = m.top.intervalSeconds
    if iv < 1 then iv = 10
    m.timer.duration = iv
    m.lastId = ""        ' force the first frame to load
    m.hasFrame = false
    m.failStart = 0

    loadNext()
    m.timer.control = "start"
end sub

sub stopCycling()
    m.timer.control = "stop"
    m.retry.control = "stop"
    hideError()
end sub

sub onTick()
    loadNext()
end sub

sub onRetry()
    loadNext()
end sub

' loadNext asks the persistent worker to fetch: set the current request inputs,
' then bump `trigger` to wake it. The worker only downloads when the image id has
' actually changed (see ImageFetcher).
sub loadNext()
    if m.top.feedUrl = invalid or m.top.feedUrl = "" then return
    m.fetcher.sourceUrl = m.top.feedUrl
    m.fetcher.idUrl = m.top.idUrl
    m.fetcher.lastId = m.lastId
    m.fetcher.trigger = m.fetcher.trigger + 1
end sub

sub onFetched()
    res = m.fetcher.result
    if res = invalid then return

    if res.ok = true
        ' Reachable again: clear any outage state and hide the error overlay
        ' immediately (switch straight back to the feed).
        m.failStart = 0
        hideError()
        if res.changed = true
            ' New image: swap it in.
            m.photo.uri = res.path
            m.lastId = res.id
            m.hasFrame = true
        else
            ' Same image as before: leave the Poster untouched (no flash) and
            ' try again in 1 second until the backend advances.
            m.retry.control = "start"
        end if
    else
        onFetchFailed()
    end if
end sub

' onFetchFailed keeps showing the last good frame for a grace period (60s of
' continuous failure) before revealing the "unreachable" message, and probes
' every second so we switch back the instant the backend returns. If we never
' managed to show a frame, there is nothing to fall back to, so show the message
' right away.
sub onFetchFailed()
    GRACE_SECONDS = 60

    if not m.hasFrame
        showError()
        m.retry.control = "start"
        return
    end if

    now = CreateObject("roDateTime").AsSeconds()
    if m.failStart = 0 then m.failStart = now

    if (now - m.failStart) >= GRACE_SECONDS
        showError()   ' down long enough: show the message
    else
        hideError()   ' grace period: keep the last frame
    end if

    m.retry.control = "start"
end sub

sub onLoadStatus()
    status = m.photo.loadStatus
    if status = "ready"
        hideError()
    else if status = "failed"
        showError()
    end if
end sub

' --- error overlay (drifts to avoid burn-in while shown) ---------------------

sub showError()
    if not m.errorBg.visible
        m.errorBg.visible = true
        moveError()
        m.burnin.control = "start"
    end if
end sub

sub hideError()
    if m.errorBg.visible
        m.errorBg.visible = false
        m.burnin.control = "stop"
    end if
end sub

sub onBurnInTick()
    moveError()
end sub

sub moveError()
    m.errorMsg.translation = m.burnSpots[m.burnIdx]
    m.burnIdx = (m.burnIdx + 1) mod m.burnSpots.Count()
end sub
