export namespace account {
	
	export class IceServer {
	    urls: string[];
	    username?: string;
	    credential?: string;
	
	    static createFrom(source: any = {}) {
	        return new IceServer(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.urls = source["urls"];
	        this.username = source["username"];
	        this.credential = source["credential"];
	    }
	}

}

export namespace main {
	
	export class AccountView {
	    gate: string;
	    email: string;
	    handle: string;
	    reason: string;
	    offline: boolean;
	
	    static createFrom(source: any = {}) {
	        return new AccountView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.gate = source["gate"];
	        this.email = source["email"];
	        this.handle = source["handle"];
	        this.reason = source["reason"];
	        this.offline = source["offline"];
	    }
	}
	export class ConvView {
	    peerId: string;
	    name: string;
	    handle: string;
	    accepted: boolean;
	    request: boolean;
	    blocked: boolean;
	    banned: boolean;
	    unread: number;
	    preview: string;
	    lastAt: number;
	    group: boolean;
	    members: number;
	    admin: boolean;
	    closed: boolean;
	    status: string;
	    kind?: string;
	    ws?: string;
	    biz?: boolean;
	    former?: boolean;
	    coworker?: boolean;
	    invitedBy?: string;
	    reports?: number;
	    pending?: number;
	
	    static createFrom(source: any = {}) {
	        return new ConvView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.peerId = source["peerId"];
	        this.name = source["name"];
	        this.handle = source["handle"];
	        this.accepted = source["accepted"];
	        this.request = source["request"];
	        this.blocked = source["blocked"];
	        this.banned = source["banned"];
	        this.unread = source["unread"];
	        this.preview = source["preview"];
	        this.lastAt = source["lastAt"];
	        this.group = source["group"];
	        this.members = source["members"];
	        this.admin = source["admin"];
	        this.closed = source["closed"];
	        this.status = source["status"];
	        this.kind = source["kind"];
	        this.ws = source["ws"];
	        this.biz = source["biz"];
	        this.former = source["former"];
	        this.coworker = source["coworker"];
	        this.invitedBy = source["invitedBy"];
	        this.reports = source["reports"];
	        this.pending = source["pending"];
	    }
	}
	export class CopyView {
	    on: boolean;
	    needsPassword: boolean;
	    savedAt: number;
	    problem: string;
	
	    static createFrom(source: any = {}) {
	        return new CopyView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.on = source["on"];
	        this.needsPassword = source["needsPassword"];
	        this.savedAt = source["savedAt"];
	        this.problem = source["problem"];
	    }
	}
	export class EvidenceView {
	    text: string;
	    sent: number;
	    where?: string;
	
	    static createFrom(source: any = {}) {
	        return new EvidenceView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.text = source["text"];
	        this.sent = source["sent"];
	        this.where = source["where"];
	    }
	}
	export class MeView {
	    handle: string;
	    username: string;
	    online: boolean;
	    peers: number;
	
	    static createFrom(source: any = {}) {
	        return new MeView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.handle = source["handle"];
	        this.username = source["username"];
	        this.online = source["online"];
	        this.peers = source["peers"];
	    }
	}
	export class MemberView {
	    peerId: string;
	    name: string;
	    admin: boolean;
	    me: boolean;
	    pending?: boolean;
	
	    static createFrom(source: any = {}) {
	        return new MemberView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.peerId = source["peerId"];
	        this.name = source["name"];
	        this.admin = source["admin"];
	        this.me = source["me"];
	        this.pending = source["pending"];
	    }
	}
	export class MsgView {
	    id: string;
	    outbound: boolean;
	    text: string;
	    sent: number;
	    delivered: boolean;
	    from?: string;
	    system?: boolean;
	    reached: number;
	    of: number;
	
	    static createFrom(source: any = {}) {
	        return new MsgView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.outbound = source["outbound"];
	        this.text = source["text"];
	        this.sent = source["sent"];
	        this.delivered = source["delivered"];
	        this.from = source["from"];
	        this.system = source["system"];
	        this.reached = source["reached"];
	        this.of = source["of"];
	    }
	}
	export class ReportView {
	    id: string;
	    from: string;
	    who: string;
	    whoId: string;
	    reason: string;
	    evidence: EvidenceView[];
	    at: number;
	
	    static createFrom(source: any = {}) {
	        return new ReportView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.from = source["from"];
	        this.who = source["who"];
	        this.whoId = source["whoId"];
	        this.reason = source["reason"];
	        this.evidence = this.convertValues(source["evidence"], EvidenceView);
	        this.at = source["at"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SettingsView {
	    background: boolean;
	    notifications: boolean;
	    startup: boolean;
	    startupAvailable: boolean;
	    trayAvailable: boolean;
	    autoUpdate: boolean;
	    version: string;
	
	    static createFrom(source: any = {}) {
	        return new SettingsView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.background = source["background"];
	        this.notifications = source["notifications"];
	        this.startup = source["startup"];
	        this.startupAvailable = source["startupAvailable"];
	        this.trayAvailable = source["trayAvailable"];
	        this.autoUpdate = source["autoUpdate"];
	        this.version = source["version"];
	    }
	}
	export class UpdateView {
	    version: string;
	    notes: string;
	    page: string;
	    ready: boolean;
	    install: string;
	
	    static createFrom(source: any = {}) {
	        return new UpdateView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.notes = source["notes"];
	        this.page = source["page"];
	        this.ready = source["ready"];
	        this.install = source["install"];
	    }
	}

}

