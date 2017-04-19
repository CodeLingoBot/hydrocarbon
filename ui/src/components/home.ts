import m from 'mithril'
import * as Mithril from 'mithril'
import nav from './nav'

export default {
	view (vnode: Mithril.Vnode<{}, {}>) {
		return m('.page', [
			m(nav),
			m('h1', "Home"),
			m('p', "This is the home page.")
		])
	}
} as Mithril.Component<{},{}>
