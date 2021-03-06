package main

import (
    "flag"
    "fmt"
    "os"
    "os/exec"
    "path/filepath"
    "strings"
    "sync"
    //"syscall"
    "time"
    log "github.com/sirupsen/logrus"
    fsnotify "github.com/fsnotify/fsnotify"
)

type DevEvent struct {
    action int
    uuid   string
    width  int
    height int
}

func (self *RunningDev) dup() ( *RunningDev ) {
    if self == nil { return nil }
    self.lock.Lock()
    dup := *self
    self.lock.Unlock()
    return &dup
}

func (self *RunningDev) setBackoff( procName string, b *Backoff, base *BaseProgs ) {
    if self == nil {
        base.lock.Lock()
        base.backoff[ procName ] = b
        base.lock.Unlock()
        return
    }
    self.lock.Lock()
    self.backoff[ procName ] = b
    self.lock.Unlock()
}

func (self *RunningDev) setProcess( procName string, b *os.Process, base *BaseProgs ) {
    if self == nil {
        base.lock.Lock()
        base.process[ procName ] = b
        base.lock.Unlock()
        return
    }
    self.lock.Lock()
    self.process[ procName ] = b
    self.lock.Unlock()
}

func (self *RunningDev) getShuttingDown( base *BaseProgs ) (bool){
    if self == nil {
        base.lock.Lock()
        sd := base.shuttingDown
        base.lock.Unlock()
        return sd
    }
    self.lock.Lock()
    sd := self.shuttingDown
    self.lock.Unlock()
    return sd
}

type RunningDev struct {
    uuid          string
    name          string
    wdaWrapper    *Launcher
    shuttingDown  bool
    lock          sync.Mutex
    failed        bool
    wdaPort       int
    vidPort       int
    devIosPort    int
    vncPort       int
    wdaStdoutPipe string
    wdaStderrPipe string
    heartbeatChan chan<- bool
    iosVersion    string
    confDup       *Config
    videoReady    bool
    streamWidth   int
    streamHeight  int
    okFirstFrame  bool
    okVidInterface bool
    okAllUp        bool
    process       map[string] *os.Process
    backoff       map[string] *Backoff
}

type BaseProgs struct {
    process    map[string] *os.Process
    backoff    map[string] *Backoff
    shuttingDown bool
    vpnLauncher *Launcher
    vpnLogWatcher *fsnotify.Watcher
    vpnLogWatcherStopChan chan<- bool
    vpnIface   string
    okStage1   bool
    okVpn      bool
    lock       sync.Mutex
}

var gStop bool

func main() {
    gStop = false

    var debug      = flag.Bool( "debug"   , false        , "Use debug log level" )
    var jsonLog    = flag.Bool( "json"    , false        , "Use json log output" )
    var vpnlist    = flag.Bool( "vpnlist" , false        , "List VPNs then exit" )
    var loadVpn    = flag.Bool( "loadVpn" , false        , "Setup / Load OpenVPN plist" )
    var unloadVpn  = flag.Bool( "unloadVpn",false        , "Unload / Remove OpenVPN plist" )
    var load       = flag.Bool( "load"    , false        , "Load Coordinator plist" )
    var unload     = flag.Bool( "unload"  , false        , "Unload Coordinator plist" )
    var addNetPerm = flag.Bool( "addNetPerm", false      , "Add network permission for coordinator app" )
    var getNetPerm = flag.Bool( "getNetPerm", false      , "Show apps with network permission" )
    var delNetPerm = flag.Bool( "delNetPerm", false      , "Delete network permission for coordinator app" )
    var configFile = flag.String( "config", "config.json", "Config file path"    )
    flag.Parse()

    if *vpnlist {
        vpns := vpns_getall()

        for vpnName, vpn := range vpns {
            fmt.Printf("Name: %s - Autoconnect: %s - %s\n", vpnName, vpn.autoConnect, vpn.state)
        }
        os.Exit(0)
    }
    
    dir, _ := filepath.Abs( filepath.Dir( os.Args[0] ) )
    changeDir := false
    if strings.HasSuffix( dir, "/Contents/MacOS" ) { // running via App
        resDir, _ := filepath.Abs( dir + "/../Resources" )
        configFileA := resDir + "/config.json"
        configFile = &configFileA
        changeDir = true
    }
    
    config := read_config( *configFile )

    if changeDir {
        os.Chdir( config.Install.RootPath )
    }
    
    if *loadVpn {
        openvpn_load( config )
        os.Exit(0)
    }
    if *unloadVpn {
        openvpn_unload( config )
        os.Exit(0)
    }
    
    if *load {
        coordinator_load( config )
        os.Exit(0)
    }
    if *unload {
        coordinator_unload( config )
        os.Exit(0)
    }
    
    if *addNetPerm {
        firewall_ensureperm( "/Applications/STF Coordinator.app" )
        os.Exit(0)
    }
    if *getNetPerm {
        firewall_showperms()
        os.Exit(0)
    }
    if *delNetPerm {
        firewall_delperm( "/Applications/STF Coordinator.app" )
        os.Exit(0)
    }
    
    if config.Install.RootPath != "" {
        os.Chdir( config.Install.RootPath )
    }
    
    useVPN := true
    if ( config.Vpn.TblickName == "none" && config.Vpn.VpnType != "openvpn" ) || config.Vpn.VpnType == "none" {
        useVPN = false
    }

    baseProgs := BaseProgs{
        process: make( map[string] *os.Process ),
        backoff: make( map[string] *Backoff ),
    }
    
    vpnEventCh := make( chan VpnEvent, 2 )
    if useVPN {
        check_vpn_status( config, &baseProgs, vpnEventCh )
    }

    lineLog, lineTracker := setup_log( config, *debug, *jsonLog )

    pubEventCh := make( chan PubEvent, 2 )

    runningDevs := make( map [string] *RunningDev )
    var devMapLock sync.Mutex
    
    devEventCh := make( chan DevEvent, 2 )
    
    coro_zmqPull( runningDevs, &devMapLock, lineLog, pubEventCh, devEventCh )
    coro_zmqReqRep( runningDevs )
    coro_zmqPub( pubEventCh )

    var ifName     string
    var curIP      string
    var vpnMissing bool
    if useVPN {
        if config.Vpn.VpnType == "tunnelblick" {
            baseProgs.okVpn = true
            baseProgs.okStage1 = true
            ifName, curIP, vpnMissing = get_net_info( config )
        } else if config.Vpn.VpnType == "openvpn" {
            baseProgs.okVpn = false
            baseProgs.okStage1 = false
        }
    } else {
        ifName = config.Network.Iface
        curIP = ifAddr( ifName )
        baseProgs.okStage1 = true
    }

    cleanup_procs( config )

    portMap := NewPortMap( config )
    
    log.WithFields( log.Fields{
        "type":     "portmap",
        "vid_ports": portMap.vidPorts,
        "wda_ports": portMap.wdaPorts,
        "vnc_ports": portMap.vncPorts,
        "dev_ios_ports": portMap.devIosPorts,
    } ).Debug("Portmap")
    
    baseProgs.shuttingDown = false

    procOptions := ProcOptions {
        config: config,
        baseProgs: &baseProgs,
        lineLog: lineLog,
    }   
    
    coro_http_server( config, devEventCh, &baseProgs, runningDevs, lineTracker )
    proc_device_trigger( procOptions )
    if config.Video.Enabled {
        proc_video_enabler( procOptions )
    }

    if useVPN {
        if vpnMissing {
            log.WithFields( log.Fields{ "type": "vpn_warn" } ).Warn("VPN not enabled; skipping start of STF")
        } else if config.Vpn.VpnType == "tunnelblick" {
            // Start provider here because tunnelblick is connected in the check_vpn_status call above
            // "stage1" is done
            proc_stf_provider( procOptions, curIP )
        }
    } else {
        proc_stf_provider( procOptions, curIP )
    }

    coro_sigterm( runningDevs, &baseProgs, config )

    // process devEvents
    event_loop( config, curIP, devEventCh, vpnEventCh, ifName, pubEventCh, runningDevs, &devMapLock, portMap, lineLog, &baseProgs )
}

func coordinator_NewLauncher( config *Config ) (*Launcher) {
    arguments := []string {
        "/Applications/STF Coordinator.app/Contents/MacOS/coordinator",
    }
    
    label := fmt.Sprintf("com.tmobile.coordinator.app")
    wd := "/Applications/STF Coordinator.app/Contents/MacOS"
    keepalive := true
    asRoot := false
    cLauncher := NewLauncher( label, arguments, keepalive, wd, asRoot )
    cLauncher.stdout = config.Log.MainApp
    //cLauncher.stderr = config.WDAWrapperStderr
    
    return cLauncher
}

func coordinator_load( config *Config ) {
    // Check that the App is installed
    // TODO
    
    // Check that the App has video permissions
    bytes, _ := exec.Command( "/usr/bin/perl", "./util/tcc.pl", "hascamera" ).Output()
    if strings.Index( string( bytes ), "no" ) != -1 {
        log.Fatal( "App does not has video permissions; run `sudo utils/tcc.pl addcamera`" )
    }
    
    // Check that the App has network permissions
    hasNetPerm := firewall_hasperm( "/Applications/STF Coordinator.app/Contents/MacOS/coordinator" )
    if !hasNetPerm {
        log.Fatal( "App does not has firewall permissions; run `sudo bin/coordinator -addNetPerm`" )
    }
    
    cLauncher := coordinator_NewLauncher( config )
    cLauncher.load()
}

func coordinator_unload( config *Config ) {
    cLauncher := coordinator_NewLauncher( config )
    cLauncher.unload()
}

func NewRunningDev( 
        gConfig *Config,
        runningDevs map[string] *RunningDev,
        devMapLock *sync.Mutex,
        portMap *PortMap,
        uuid string ) ( *RunningDev ) {
        
    wdaPort, vidPort, devIosPort, vncPort, _, _, config := assign_ports( gConfig, portMap )
  
    devd := RunningDev{
        uuid: uuid,
        shuttingDown:  false,
        failed:        false,
        wdaPort:       wdaPort,
        vidPort:       vidPort,
        devIosPort:    devIosPort,
        vncPort:       vncPort,
        confDup:       config,
        videoReady:    false,
        streamWidth:   0,
        streamHeight:  0,
        okFirstFrame:  true,
        okVidInterface: false,
        okAllUp:        false,
        process: make( map[string] *os.Process ),
        backoff: make( map[string] *Backoff ),
    }
    
    devd.name = getDeviceName( uuid )
    
    devMapLock.Lock()
    runningDevs[uuid] = &devd
    devMapLock.Unlock()
    
    log.WithFields( log.Fields{
        "type":     "devd_create",
        "dev_name": devd.name,
        "dev_uuid": uuid,
        "vid_port": vidPort,
        "wda_port": wdaPort,
        "vnc_port": vncPort,
        "dev_ios_port": devIosPort,
    } ).Info("Device object created")
    
    return &devd
}

func event_loop(
        gConfig     *Config,
        curIP       string,
        devEventCh  <-chan DevEvent,
        vpnEventCh  <-chan VpnEvent,
        tunName     string,
        pubEventCh  chan<- PubEvent,
        runningDevs map[string] *RunningDev,
        devMapLock *sync.Mutex,
        portMap     *PortMap,
        lineLog     *log.Entry,
        baseProgs *BaseProgs ) {
    
    gProcOptions := ProcOptions {
        config: gConfig,
        baseProgs: baseProgs,
        lineLog: lineLog,
    }

    if baseProgs.okStage1 == false {
        for {
            vpnEvent := <- vpnEventCh
            if vpnEvent.action == 0 {
                tunName = vpnEvent.text1
                curIP = ifAddr( tunName )
                baseProgs.okVpn = true
            }
            
            if baseProgs.okVpn == true && baseProgs.okStage1 == false {
                proc_stf_provider( gProcOptions, curIP )
                baseProgs.okStage1 = true
                break
            }
        }
    }

    log.WithFields( log.Fields{
        "type":     "event_loop_start",
    } ).Debug("Event loop start")
    
    for {
        select {
        case vpnEvent := <- vpnEventCh:
            if vpnEvent.action == 0 {
                //iface := vpnEvent.text1
                // do nothing; assume tunnel interface is unchanged
            }
            
        case devEvent := <- devEventCh:
            uuid := devEvent.uuid
    
            var devd *RunningDev = nil
            var ok = false
            
            o := ProcOptions {
                devd: devd,
                baseProgs: baseProgs,
                lineLog: lineLog,
            }
            
            devMapLock.Lock()
            if devd, ok = runningDevs[uuid]; !ok {
                devMapLock.Unlock()
                devd = NewRunningDev( gConfig, runningDevs, devMapLock, portMap, uuid )
            } else {
                devMapLock.Unlock()
            }
            o.devd = devd
            
            if devEvent.action == 0 { // device connect
                devName := devd.name
    
                log.WithFields( log.Fields{
                    "type":     "dev_connect",
                    "dev_name": devName,
                    "dev_uuid": uuid,                
                } ).Info("Device connected")
    
                o.config = devd.confDup
                
                if o.config.Video.Enabled {
                    proc_h264_to_jpeg( o )
                    proc_ios_video_stream( o, tunName )
                }
            }
            if devEvent.action == 1 { // device disconnect
                log.WithFields( log.Fields{
                    "type":     "dev_disconnect",
                    "dev_name": devd.name,
                    "dev_uuid": uuid,
                } ).Info("Device disconnected")
    
                // send true to the stop heartbeat channel
                
                closeRunningDev( devd, portMap )
    
                devMapLock.Lock()
                delete( runningDevs, uuid )
                devMapLock.Unlock()
                devd = nil
                
                // Notify stf that the device is gone
                pubEvent := PubEvent{}
                pubEvent.action  = devEvent.action
                pubEvent.uuid    = devEvent.uuid
                pubEvent.name    = ""
                pubEvent.wdaPort = 0
                pubEvent.vidPort = 0
                pubEventCh <- pubEvent
            }
            if devEvent.action == 2 { // video interface available
                log.WithFields( log.Fields{
                    "type":     "vid_interface",
                    "dev_name": devd.name,
                    "dev_uuid": uuid,
                } ).Info("Video - interface available")
                devd.okVidInterface = true
            }
            if devEvent.action == 3 { // first video frame
                devd.okFirstFrame = true
                devd.streamWidth = devEvent.width
                devd.streamHeight = devEvent.height
                log.WithFields( log.Fields{
                    "type": "first_frame",
                    "proc": "mirrorfeed",
                    "width": devEvent.width,
                    "height": devEvent.height,
                    "uuid": uuid,
                } ).Info("Video - first frame")
            }
            
           if devd != nil && devd.okAllUp == false {
                o.config = devd.confDup
                
                if !o.config.Video.Enabled || ( o.devd.okVidInterface == true && o.devd.okFirstFrame == true ) {
                    o.devd.okAllUp = true
                    continue_dev_start( o, curIP )
                }
            }
        }
    }
}

func continue_dev_start( o ProcOptions, curIP string ) {
    uuid := o.devd.uuid
    
    if o.config.Video.Enabled && o.config.Video.UseVnc {
        proc_vnc_proxy( o )
    }
    
    time.Sleep( time.Second * 2 )
    
    iosVersion := getDeviceInfo( uuid, "ProductVersion" )

    log.WithFields( log.Fields{
        "type":     "ios_version",
        "dev_name": o.devd.name,
        "dev_uuid": uuid,
        "ios_version": iosVersion,
    } ).Debug("IOS Version")
    
    start_proc_wdaproxy( o, uuid, iosVersion )
    
    time.Sleep( time.Second * 3 )

    proc_device_ios_unit( o, uuid, curIP )
}