sub init()
    m.top.backgroundURI = ""
    m.top.backgroundColor = "0x000000FF"

    m.view = m.top.findNode("view")
    m.hintGroup = m.top.findNode("hintGroup")
    m.settings = m.top.findNode("settings")
    m.hl = m.top.findNode("hl")
    m.srhint = m.top.findNode("srhint")
    m.rows = [
        m.top.findNode("row0"),
        m.top.findNode("row1"),
        m.top.findNode("row2"),
        m.top.findNode("row3"),
        m.top.findNode("row4")
    ]
    m.rowY = [302, 382, 462, 542, 622]
    m.sel = 0
    m.inSettings = false

    m.rows[4].text = "Save"
    m.srhint.text = "Up / Down to move    OK to change    Back to return to preview"

    ' Start the live feed (this is both the screensaver and the manual preview).
    applyConfigToView()
    m.view.running = true

    m.top.observeField("interactive", "onInteractiveChanged")
    m.top.observeField("openSettings", "onOpenSettingsChanged")
    ' NOTE: do not setFocus here. A real screensaver runs in a non-interactive
    ' context where taking focus can leave it blank; focus is only needed for the
    ' settings overlay, so we take it when interactive (see onInteractiveChanged).
end sub

sub applyConfigToView()
    cfg = ssGetConfig()
    m.view.feedUrl = ssFeedUrl(cfg)
    m.view.idUrl = ssIdUrl(cfg)
    m.view.intervalSeconds = cfg.interval
end sub

sub onInteractiveChanged()
    updateHint()
    ' Take focus only for the interactive (manual) launch so key navigation works.
    if m.top.interactive then m.top.setFocus(true)
end sub

sub onOpenSettingsChanged()
    if m.top.openSettings then openSettings()
end sub

sub updateHint()
    m.hintGroup.visible = (m.top.interactive and not m.inSettings)
end sub

' ---- input -----------------------------------------------------------------

function onKeyEvent(key as string, press as boolean) as boolean
    if not press then return false

    if m.inSettings
        return settingsKey(key)
    end if

    ' Preview mode: only a manual (interactive) launch can open settings via *.
    if m.top.interactive and key = "options"
        openSettings()
        return true
    end if
    return false   ' e.g. Back exits the channel
end function

function settingsKey(key as string) as boolean
    if key = "down"
        m.sel = (m.sel + 1) mod 5
        moveHighlight()
    else if key = "up"
        m.sel = (m.sel + 4) mod 5
        moveHighlight()
    else if key = "OK"
        onSelect()
    else if key = "back"
        closeSettings()   ' Back returns to preview without saving
    end if
    return true            ' swallow all keys while the overlay is up
end function

' ---- settings overlay ------------------------------------------------------

sub openSettings()
    m.cfg = ssGetConfig()
    m.sel = 0
    refreshLabels()
    moveHighlight()
    m.settings.visible = true
    m.inSettings = true
    updateHint()
end sub

sub closeSettings()
    m.settings.visible = false
    m.inSettings = false
    updateHint()
    ' Apply the (possibly just-saved) settings to the live feed.
    applyConfigToView()
    m.view.running = true   ' alwaysNotify restarts cycling with the new config
end sub

sub onSelect()
    if m.sel = 0
        if m.cfg.protocol = "http"
            m.cfg.protocol = "https"
        else
            m.cfg.protocol = "http"
        end if
        refreshLabels()
    else if m.sel = 1
        showKeyboard("host", "Server address (IP or hostname)", m.cfg.host)
    else if m.sel = 2
        showKeyboard("port", "Server port", m.cfg.port)
    else if m.sel = 3
        cycleInterval()
    else if m.sel = 4
        ssSaveConfig(m.cfg)
        closeSettings()   ' Save returns to preview
    end if
end sub

sub refreshLabels()
    m.rows[0].text = "Protocol:   " + m.cfg.protocol
    m.rows[1].text = "Address:    " + m.cfg.host
    m.rows[2].text = "Port:       " + m.cfg.port
    m.rows[3].text = "Interval:   " + m.cfg.interval.ToStr() + "s"
end sub

sub moveHighlight()
    m.hl.translation = [150, m.rowY[m.sel] - 8]
end sub

' cycleInterval steps 10s -> 30s -> 60s, then opens the keyboard for a custom
' value; any custom value wraps back to 10s.
sub cycleInterval()
    iv = m.cfg.interval
    if iv = 10
        m.cfg.interval = 30
        refreshLabels()
    else if iv = 30
        m.cfg.interval = 60
        refreshLabels()
    else if iv = 60
        showKeyboard("interval", "Custom interval in seconds", iv.ToStr())
    else
        m.cfg.interval = 10
        refreshLabels()
    end if
end sub

sub showKeyboard(field as string, title as string, current as string)
    m.editField = field
    kb = CreateObject("roSGNode", "KeyboardDialog")
    kb.title = title
    kb.text = current
    kb.buttons = ["Save", "Cancel"]
    m.kb = kb
    kb.observeField("buttonSelected", "onKbButton")
    kb.observeField("wasClosed", "onKbClosed")
    m.top.dialog = kb
end sub

sub onKbButton()
    if m.kb.buttonSelected = 0   ' Save
        v = m.kb.text
        if m.editField = "host"
            if v <> "" then m.cfg.host = v
        else if m.editField = "port"
            if v <> "" then m.cfg.port = v
        else if m.editField = "interval"
            n = Int(Val(v))
            if n < 1 then n = 1
            m.cfg.interval = n
        end if
        refreshLabels()
    end if
    m.top.dialog = invalid
end sub

sub onKbClosed()
    m.top.setFocus(true)   ' return focus to the scene for navigation
end sub
