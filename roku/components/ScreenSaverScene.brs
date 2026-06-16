sub init()
    m.top.backgroundURI = ""
    m.top.backgroundColor = "0x000000FF"

    ' Config comes from the device registry (set via the settings overlay),
    ' falling back to the defaults in config.brs.
    cfg = ssGetConfig()

    m.view = m.top.findNode("view")
    m.view.feedUrl = ssFeedUrl(cfg)
    m.view.idUrl = ssIdUrl(cfg)
    m.view.intervalSeconds = cfg.interval
    m.view.running = true
end sub
