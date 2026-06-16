' Entry points for the channel.
'
' RunScreenSaver()  - the OS runs this when the TV goes idle. Uses the minimal
'                     ScreenSaverScene (just the feed) — the richer interactive
'                     scene does not render in the screensaver context.
' Main()            - launching the channel tile shows the feed as a live
'                     "preview" (PreviewScene) plus a "press * for Settings"
'                     hint; * opens the settings overlay, Save returns to preview.
' RunScreenSaverSettings() - the OS "Change screensaver settings" menu; opens
'                     PreviewScene with the settings overlay already up (renders
'                     blank on some firmware, so the tile is the reliable way in).

sub RunScreenSaver()
    launch("ScreenSaverScene", false, false)
end sub

sub Main()
    launch("PreviewScene", true, false)
end sub

sub RunScreenSaverSettings()
    launch("PreviewScene", true, true)
end sub

sub launch(sceneName as string, interactive as boolean, openSettings as boolean)
    screen = CreateObject("roSGScreen")
    port = CreateObject("roMessagePort")
    screen.setMessagePort(port)

    scene = screen.CreateScene(sceneName)
    ' Only PreviewScene has these fields; set them only for the interactive launch.
    if interactive
        scene.interactive = true
        if openSettings then scene.openSettings = true
    end if
    screen.show()

    while true
        msg = wait(0, port)
        if type(msg) = "roSGScreenEvent"
            if msg.isScreenClosed() then return
        end if
    end while
end sub
