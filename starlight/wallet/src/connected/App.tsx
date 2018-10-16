import * as React from 'react'
import { Dispatch } from 'redux'
import { Route } from 'react-router'
import { connect } from 'react-redux'

import { ApplicationState } from 'schema'
import { ConfigLanding } from 'components/ConfigLanding'
import { ConnectedEventLoop } from 'connected/EventLoop'
import { ConnectedInitConfig } from 'components/forms/InitConfig'
import { ConnectedLoginForm } from 'components/forms/LoginForm'
import { Login } from 'components/Login'
import { Navigation } from 'components/Navigation'
import { lifecycle } from 'state/lifecycle'
import { hot } from 'react-hot-loader'

interface Props {
  isConfigured: boolean
  isLoggedIn: boolean
  status: () => any
}

class App extends React.Component<Props, {}> {
  public async componentDidMount() {
    this.props.status()
  }

  public render() {
    if (this.props.isLoggedIn) {
      return (
        <ConnectedEventLoop>
          <Route path="/" component={Navigation} />
        </ConnectedEventLoop>
      )
    } else if (this.props.isConfigured) {
      return (
        <Route
          path="/"
          render={props => <Login {...props} form={<ConnectedLoginForm />} />}
        />
      )
    } else {
      return (
        <Route
          path="/"
          render={props => (
            <ConfigLanding {...props} form={<ConnectedInitConfig />} />
          )}
        />
      )
    }
  }
}

const mapStateToProps = (state: ApplicationState) => {
  return {
    isConfigured: state.lifecycle.isConfigured,
    isLoggedIn: state.lifecycle.isLoggedIn,
  }
}
const mapDispatchToProps = (dispatch: Dispatch) => {
  return {
    status: () => lifecycle.status(dispatch),
  }
}
export const ConnectedApp = connect(
  mapStateToProps,
  mapDispatchToProps
)(hot(module)(App))