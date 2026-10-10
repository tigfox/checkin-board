package web

// routes is every API endpoint (spec 7.1) and who may call it. The
// access test walks this table, so a route can't be added unguarded.
func (s *server) routes() []route {
	return []route{
		// Logins (spec 7.2).
		{method: "GET", pattern: "/api/setup", access: public, h: s.getSetup},
		{method: "POST", pattern: "/api/setup", access: public, h: s.postSetup},
		{method: "POST", pattern: "/api/login", access: public, h: s.postLogin},
		{method: "POST", pattern: "/api/logout", access: public, h: s.postLogout},
		{method: "GET", pattern: "/api/session", access: volunteer, h: s.getSession},

		// Volunteer interface.
		{method: "GET", pattern: "/api/station", access: volunteer, h: s.getStation},
		{method: "GET", pattern: "/api/entries", access: volunteer, h: s.getEntries},
		{method: "POST", pattern: "/api/entries", access: volunteer, h: s.postEntry},
		{method: "DELETE", pattern: "/api/entries/{id}", access: volunteer, h: s.deleteEntry},
		{method: "GET", pattern: "/api/clock", access: volunteer, h: s.getClock},
		{method: "POST", pattern: "/api/clock/sync", access: volunteer, h: s.postClockSync},
		{method: "GET", pattern: "/api/branding", access: volunteer, h: s.getBranding},
		{method: "GET", pattern: "/api/branding/logo", access: volunteer, h: s.getLogo},

		// Admin: settings and graywolf.
		{method: "GET", pattern: "/api/admin/settings", access: admin, h: s.getSettings},
		{method: "PUT", pattern: "/api/admin/settings", access: admin, h: s.putSettings},
		{method: "PUT", pattern: "/api/admin/settings/race", access: admin, h: s.putRaceSettings},
		{method: "PUT", pattern: "/api/admin/settings/messaging", access: admin, h: s.putMessagingSettings},
		{method: "PUT", pattern: "/api/admin/callsign", access: admin, h: s.putCallsign},
		{method: "GET", pattern: "/api/admin/gw", access: admin, h: s.getGraywolf},
		{method: "GET", pattern: "/api/admin/radio", access: admin, h: s.getRadio},
		// Race config files (phase 12a).
		{method: "GET", pattern: "/api/admin/raceconfigs", access: admin, h: s.getRaceConfigs},
		{method: "POST", pattern: "/api/admin/raceconfigs", access: admin, h: s.postRaceConfig, upload: true},
		{method: "GET", pattern: "/api/admin/raceconfigs/{name}", access: admin, h: s.getRaceConfig},
		{method: "DELETE", pattern: "/api/admin/raceconfigs/{name}", access: admin, h: s.deleteRaceConfig},
		{method: "POST", pattern: "/api/admin/raceconfigs/{name}/preview", access: admin, h: s.postRaceConfigPreview},
		{method: "POST", pattern: "/api/admin/raceconfigs/{name}/apply", access: admin, h: s.postRaceConfigApply},
		{method: "GET", pattern: "/api/admin/raceconfig/export", access: admin, h: s.getRaceConfigExport},
		{method: "POST", pattern: "/api/admin/graywolf/restore", access: admin, h: s.postGraywolfRestore},
		{method: "GET", pattern: "/api/eventpage", access: volunteer, h: s.getEventPage},
		// 64 KB of text can need more than that as escaped JSON.
		{method: "PUT", pattern: "/api/admin/eventpage", access: admin, h: s.putEventPage, maxBody: 512 << 10},
		// The local configuration guide (an admin page, not an API).
		{method: "GET", pattern: "/guide", access: admin, page: true, h: s.getGuide},
		{method: "GET", pattern: "/api/admin/peers", access: admin, h: s.getPeers},
		{method: "POST", pattern: "/api/admin/peers/restore", access: admin, h: s.postPeersRestore},
		{method: "POST", pattern: "/api/admin/inbox/reread", access: admin, h: s.postInboxReread},
		{method: "PUT", pattern: "/api/admin/password/admin", access: admin, h: s.putAdminPassword},
		{method: "PUT", pattern: "/api/admin/password/volunteer", access: admin, h: s.putVolunteerPassword},

		// Admin: race lifecycle (spec 4.7).
		{method: "POST", pattern: "/api/admin/race/start", access: admin, h: s.postStart},
		{method: "POST", pattern: "/api/admin/race/complete", access: admin, h: s.postComplete},
		{method: "POST", pattern: "/api/admin/race/secure", access: admin, h: s.postSecure},
		{method: "POST", pattern: "/api/admin/race/check-in", access: admin, h: s.postCheckIn},
		{method: "POST", pattern: "/api/admin/race/cleanup-graywolf", access: admin, h: s.postCleanup},

		// Admin: deployment link check (spec 4.8).
		{method: "GET", pattern: "/api/admin/linkcheck", access: admin, h: s.getLinkChecks},
		{method: "POST", pattern: "/api/admin/linkcheck", access: admin, h: s.postLinkCheck},
		{method: "POST", pattern: "/api/admin/linkcheck/{id}/cancel", access: admin, h: s.postLinkCheckCancel},
		{method: "GET", pattern: "/api/admin/linkcheck/readiness", access: admin, h: s.getLinkReadiness},

		// Admin: node panel (8.4).
		{method: "GET", pattern: "/api/admin/panel", access: admin, h: s.getPanel},
		{method: "PUT", pattern: "/api/admin/panel", access: admin, h: s.putPanel},
		{method: "GET", pattern: "/api/admin/panel/menu", access: admin, h: s.getPanelMenu},
		{method: "PUT", pattern: "/api/admin/panel/menu", access: admin, h: s.putPanelMenu},
		{method: "POST", pattern: "/api/admin/panel/menu/reset", access: admin, h: s.postPanelMenuReset},
		{method: "GET", pattern: "/api/admin/panel/preview.png", access: admin, h: s.getPanelPreview},
		{method: "POST", pattern: "/api/admin/panel/refresh", access: admin, h: s.postPanelRefresh},
		{method: "POST", pattern: "/api/admin/panel/test-pattern", access: admin, h: s.postPanelTestPattern},
		{method: "POST", pattern: "/api/admin/panel/detect", access: admin, h: s.postPanelDetect},

		// Local automation (graywolf webhook Action, node panel), token from loopback only.
		{method: "POST", pattern: "/api/hook/linkcheck", access: hook, h: s.postHookLinkCheck},
		{method: "GET", pattern: "/api/hook/panel", access: hook, h: s.getHookPanel},
		{method: "POST", pattern: "/api/hook/panel/actions/{id}", access: hook, h: s.postHookPanelAction},
		{method: "POST", pattern: "/api/hook/panel/refreshed", access: hook, h: s.postHookPanelRefreshed},
		{method: "PUT", pattern: "/api/hook/panel/controller", access: hook, h: s.putHookPanelController},
		{method: "POST", pattern: "/api/hook/panel/detect-gave-up", access: hook, h: s.postHookPanelGaveUp},
		{method: "POST", pattern: "/api/admin/race/reset", access: admin, h: s.postReset},

		// Admin: checkpoint tools.
		{method: "GET", pattern: "/api/admin/outbox", access: admin, h: s.getOutbox},
		{method: "GET", pattern: "/api/admin/recovery/export.csv", access: admin, h: s.getRecoveryExport},

		// Admin: HQ tools.
		{method: "GET", pattern: "/api/admin/board", access: admin, h: s.getBoard},
		{method: "GET", pattern: "/api/admin/status", access: admin, h: s.getStatus},
		{method: "POST", pattern: "/api/admin/status/{cp}/rerequest", access: admin, h: s.postRerequest},
		{method: "GET", pattern: "/api/admin/export.csv", access: admin, h: s.getResultsExport},
		{method: "GET", pattern: "/api/admin/checkpoints", access: admin, h: s.getCheckpoints},
		{method: "POST", pattern: "/api/admin/checkpoints", access: admin, h: s.postCheckpoint},
		{method: "PUT", pattern: "/api/admin/checkpoints/{id}", access: admin, h: s.putCheckpoint},
		{method: "DELETE", pattern: "/api/admin/checkpoints/{id}", access: admin, h: s.deleteCheckpoint},
		{method: "GET", pattern: "/api/admin/runners", access: admin, h: s.getRunners},
		{method: "POST", pattern: "/api/admin/runners", access: admin, h: s.postRunner},
		{method: "DELETE", pattern: "/api/admin/runners/{bib}", access: admin, h: s.deleteRunner},
		{method: "GET", pattern: "/api/admin/runners/{bib}/history", access: admin, h: s.getRunnerHistory},
		{method: "POST", pattern: "/api/admin/runners/import", access: admin, h: s.postRunnerImport, upload: true},
		{method: "POST", pattern: "/api/admin/recovery/import", access: admin, h: s.postRecoveryImport, upload: true},
		{method: "POST", pattern: "/api/admin/recovery/journal", access: admin, h: s.postJournalImport, upload: true},

		// Admin: board branding (spec 8.3).
		{method: "GET", pattern: "/api/admin/branding", access: admin, h: s.getAdminBranding},
		{method: "PUT", pattern: "/api/admin/branding", access: admin, h: s.putBranding},
		{method: "POST", pattern: "/api/admin/branding/check", access: admin, h: s.postBrandingCheck},
		{method: "PUT", pattern: "/api/admin/branding/logo", access: admin, h: s.putLogo, upload: true},
		{method: "DELETE", pattern: "/api/admin/branding/logo", access: admin, h: s.deleteLogo},
	}
}
