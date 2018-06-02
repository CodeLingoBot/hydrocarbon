import { h, Component } from "preact";
import { route, Router } from "preact-router";
import { bind } from "decko";

import Header from "@/components/navbar";
import Home from "@/routes/home";
import Feed from "@/routes/feed";
import Settings from "@/routes/settings";
import Login from "@/routes/login";
import Callback from "@/routes/callback";

import style from "./style.css";

require("preact/devtools");

const apiKeyName = "hc-api-key";
const emailKeyName = "hc-email";

export default class Layout extends Component {
  constructor(props) {
    super(props);

    let email = null;
    let apiKey = null;
    apiKey = window.localStorage.getItem(apiKeyName);
    email = window.localStorage.getItem(emailKeyName);

    this.setState({
      email,
      apiKey
    });
  }

  @bind
  login(email, apiKey) {
    window.localStorage.setItem(apiKeyName, apiKey);
    window.localStorage.setItem(emailKeyName, email);

    this.setState({
      email,
      apiKey
    });
  }

  @bind
  logout() {
    window.localStorage.removeItem(apiKeyName);
    window.localStorage.removeItem(emailKeyName);

    this.setState({ email: null, apiKey: null });

    route("/login");
  }

  render({}, { email, apiKey }) {
    return (
      <div class={style.layout}>
        <Header email={email} logoutCallback={this.logout} />
        <Router>
          <Home path="/" />
          <Feed path="/feed/:folderId?/:feedId?/:postId?" />
          <Settings path="/settings" />
          <Login path="/login" />
          <Callback path="/callback" loginCallback={this.login} />
        </Router>
      </div>
    );
  }
}
