# How to Get a VK Token

## Via OAuth (Recommended)

1. Open this URL in your browser:

   ```
   https://oauth.vk.com/authorize?client_id=6287487&display=page&redirect_uri=https://oauth.vk.com/blank.html&scope=offline&response_type=token&v=5.274
   ```

2. Click "Allow" to authorize
3. You'll be redirected to a URL like:
   ```
   https://oauth.vk.com/blank.html#access_token=vk1.a.XXXXX&expires_in=0&user_id=12345
   ```
4. Copy the `access_token` value (starts with `vk1.a.`)

## Using Your Own App ID

1. Go to https://vk.com/editapp?act=create
2. Create a Standalone app
3. Note the App ID
4. Use the URL above but replace `client_id=6287487` with your App ID

## Usage

```bash
# Single token
callvpn-client --link=<link> --vk-token='vk1.a.XXXXX'

# Multiple tokens (different accounts)
callvpn-client --link=<link> --vk-token='vk1.a.TOKEN1' --vk-token='vk1.a.TOKEN2'

# Via environment variable
export VK_TOKENS='vk1.a.TOKEN1,vk1.a.TOKEN2'
callvpn-client --link=<link>
```

## Shell Escaping

If using OK auth_tokens (starting with `$`), use single quotes to prevent shell expansion:
```bash
--vk-token='$xCUTeFZFtk...'
```

## Token Lifetime

- **VK access_token** (`vk1.a.*`): permanent with `offline` scope
- **OK auth_token** (`$*`): unknown lifetime, may expire — use VK tokens when possible
