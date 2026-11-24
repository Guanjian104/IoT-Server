-- 1.0pro 增加 audio 主题后，eco-iot 要能订上报。
-- 设备自己的 audio 发布和订阅、eco-iot 对具体设备的 audio 发布，在 render-emqx.sh 的允许查询里按 device 表现查。
-- 这份只补订阅行。已经有的行不会重复插入。

INSERT IGNORE INTO mqtt_acl (username, clientid, permission, action, topic) VALUES
  ('eco-iot', '', 'allow', 'subscribe', 'pet/collar/1_0pro/+/audio'),
  ('eco-iot', '', 'allow', 'subscribe', 'pet/collar/2_0_test/+/audio');
