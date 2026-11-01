import accountRefill from './accountRefill'
import ovhIPv6 from './ovhIPv6'
import adminAccessKeys from './adminAccessKeys'
import happyFirstToken from './happyFirstToken'
import landing from './landing'
import common from './common'
import dashboard from './dashboard'
import channelMonitorV2 from './channelMonitorV2'
import batchImage from './batchImage'
import admin from './admin'
import misc from './misc'

export default {
  accountRefill,
  ovhIPv6,
  happyFirstToken,
  adminAccessKeys,
  ...landing,
  ...common,
  ...dashboard,
  ...channelMonitorV2,
  ...batchImage,
  admin,
  ...misc,
}
