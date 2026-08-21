<div align="center">
  <a href="README.md"><b>GoQuorra</b></a>
</div>

# Terms and Conditions

These terms govern your use of GoQuorra, an open source job queue (the
"Software").
By using, building, running, or contributing to the Software, you agree to
these terms.
If you do not agree, do not use the Software.

## 1. Licence

The Software is licensed under the MIT License, in the [LICENSE](LICENSE)
file.
That licence is the authoritative statement of your rights to use, copy,
modify, and distribute the Software.
If anything here conflicts with the LICENSE file, the LICENSE file governs.

## 2. No warranty

The Software is provided "as is", without warranty of any kind, express or
implied.
This includes the warranties of merchantability, fitness for a particular
purpose, and noninfringement.
You use the Software at your own risk.

## 3. Limitation of liability

To the maximum extent that the law permits, the authors and copyright holders
are not liable for any claim, damage, or other liability, whether in an action
of contract, tort, or otherwise, arising from or connected with the Software
or its use.

This matters more for this project than for most.
The Software queues work on your behalf.
It delivers a job at least once and can deliver it more than once, it can lose
a job if the database it is pointed at loses data, and a defect in it can
leave work undone.
You are responsible for deciding whether it suits what you are doing.

## 4. You run it

You are responsible for the deployment, for the database behind it, for the
network it runs on, and for the API key that guards it.
The Software refuses to start without a key and does not choose one for you.

## 5. Your data

The authors of the Software operate no service and receive nothing.
Every job, payload, and log line stays on the systems you run.
The Software sends nothing anywhere except to the database you name and to the
workers that connect to it.

## 6. Contributions

You keep the copyright in what you contribute.
By opening a pull request you agree that your contribution is licensed under
the MIT License, on the same terms as the rest of the Software.

## 7. Changes

These terms can change.
The version in the repository at the time you use a copy is the version that
applies to it.
