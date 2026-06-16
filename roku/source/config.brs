' Shared configuration for the screensaver. Functions defined in source/ are
' global and callable from any component.
'
' These are the out-of-the-box DEFAULTS. Users can override them on-device via
' Settings > Screen saver > Change screensaver settings (see SettingsScene),
' which persists the values in the device registry.

function ssDefaults() as object
    return {
        protocol: "http"          ' http | https
        host:     "192.168.1.1"   ' backend IP or hostname
        port:     "9543"          ' backend port
        interval: 10              ' fetch interval in seconds
    }
end function

' ssGetConfig returns the effective config: registry values where present,
' otherwise the defaults above.
function ssGetConfig() as object
    d = ssDefaults()
    sec = CreateObject("roRegistrySection", "screensaver")
    return {
        protocol: ssReadStr(sec, "protocol", d.protocol)
        host:     ssReadStr(sec, "host", d.host)
        port:     ssReadStr(sec, "port", d.port)
        interval: ssReadInt(sec, "interval", d.interval)
    }
end function

sub ssSaveConfig(cfg as object)
    sec = CreateObject("roRegistrySection", "screensaver")
    sec.Write("protocol", cfg.protocol)
    sec.Write("host", cfg.host)
    sec.Write("port", cfg.port)
    sec.Write("interval", cfg.interval.ToStr())
    sec.Flush()
end sub

' ssFeedUrl builds the full image URL from a config object.
function ssFeedUrl(cfg as object) as string
    return cfg.protocol + "://" + cfg.host + ":" + cfg.port + "/feed.jpg"
end function

' ssIdUrl builds the URL of the cheap content-hash endpoint used to detect when
' the served image actually changes (avoids re-loading the same frame).
function ssIdUrl(cfg as object) as string
    return cfg.protocol + "://" + cfg.host + ":" + cfg.port + "/feed.id"
end function

function ssReadStr(sec as object, key as string, default as string) as string
    if sec.Exists(key)
        v = sec.Read(key)
        if v <> invalid and v <> "" then return v
    end if
    return default
end function

function ssReadInt(sec as object, key as string, default as integer) as integer
    if sec.Exists(key)
        v = sec.Read(key)
        if v <> invalid and v <> ""
            n = Int(Val(v))
            if n > 0 then return n
        end if
    end if
    return default
end function
