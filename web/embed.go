// Package web embeds the console's static assets.
package web

import "embed"

// FS holds index.html and its scripts and styles.
//
//go:embed index.html style.css app.js dial.js detail.js forge.js util.js i18n.js confirm.js todos.js trust.js notify.js tour.js settings.js drafts.js migrate.js favicon.svg
var FS embed.FS
