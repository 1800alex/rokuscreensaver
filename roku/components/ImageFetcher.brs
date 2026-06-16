sub init()
    m.top.functionName = "run"
end sub

' run is a single long-lived loop. It reuses two roUrlTransfer objects (one for
' the id check, one for the image) across every request, so we never accumulate
' tasks or sockets. SetMinimumTransferRate makes a stalled request abort instead
' of blocking the worker forever — so a flaky connection recovers on the next
' trigger rather than wedging networking for good.
'
' It wakes on a `trigger` change OR a short timeout and re-checks, so the very
' first request can't be missed by a race with the field observer.
sub run()
    m.port = CreateObject("roMessagePort")
    m.top.observeField("trigger", m.port)

    m.xferId = CreateObject("roUrlTransfer")
    m.xferImg = CreateObject("roUrlTransfer")
    m.xferId.setMinimumTransferRate(1, 15)   ' abort the id check if stalled >15s
    m.xferImg.setMinimumTransferRate(1, 25)  ' abort the image fetch if stalled >25s

    m.imgNo = 0
    m.seen = 0

    while true
        wait(500, m.port)
        if m.top.trigger <> m.seen
            m.seen = m.top.trigger
            doFetch()
        end if
    end while
end sub

' doFetch checks the backend's current image id and downloads the image only if
' it changed. Reports {ok, changed, id, path} on the result field. On a change it
' writes a uniquely-named tmp file (so the Poster never reuses a cached URI) and
' deletes the one from two fetches ago to bound tmp: usage.
sub doFetch()
    feedUrl = m.top.sourceUrl
    if feedUrl = invalid or feedUrl = "" then return

    newId = httpGetString(m.xferId, m.top.idUrl)
    if newId <> "" and newId = m.top.lastId
        m.top.result = { ok: true, changed: false, id: newId, path: "" }
        return
    end if

    m.imgNo = m.imgNo + 1
    dest = "tmp:/saver_" + m.imgNo.ToStr() + ".jpg"
    code = httpGetFile(m.xferImg, feedUrl, dest)

    if code >= 200 and code < 300
        DeleteFile("tmp:/saver_" + (m.imgNo - 2).ToStr() + ".jpg")
        reportId = newId
        if reportId = "" then reportId = m.top.lastId   ' id endpoint unavailable
        m.top.result = { ok: true, changed: true, id: reportId, path: dest }
    else
        m.top.result = { ok: false, changed: false, id: m.top.lastId, path: "" }
    end if
end sub

function httpGetString(xfer as object, url as dynamic) as string
    if url = invalid or url = "" then return ""
    xfer.setUrl(url)
    applyCerts(xfer, url)
    return xfer.getToString()
end function

function httpGetFile(xfer as object, url as string, dest as string) as integer
    xfer.setUrl(url)
    applyCerts(xfer, url)
    return xfer.getToFile(dest)
end function

sub applyCerts(xfer as object, url as string)
    ' HTTPS needs the device cert bundle; harmless to skip for plain http LANs.
    if Left(LCase(url), 5) = "https"
        xfer.setCertificatesFile("common:/certs/ca-bundle.crt")
        xfer.initClientCertificates()
    end if
end sub
